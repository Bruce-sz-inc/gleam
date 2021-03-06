// Package agent runs on servers with computing resources, and executes
// tasks sent by driver.
package agent

import (
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/chrislusf/gleam/idl/master_rpc"
	"github.com/chrislusf/gleam/msg"
	"github.com/chrislusf/gleam/util"
	"github.com/golang/protobuf/proto"
	"google.golang.org/grpc"
)

type AgentServerOption struct {
	Master       *string
	Host         *string
	Port         *int
	Dir          *string
	DataCenter   *string
	Rack         *string
	MaxExecutor  *int
	MemoryMB     *int64
	CPULevel     *int
	CleanRestart *bool
}

type AgentServer struct {
	Option                *AgentServerOption
	Master                string
	wg                    sync.WaitGroup
	listener              net.Listener
	computeResource       *pb.ComputeResource
	allocatedResource     *pb.ComputeResource
	allocatedResourceLock sync.Mutex
	storageBackend        *LocalDatasetShardsManager
	inMemoryChannels      *LocalDatasetShardsManagerInMemory
	localExecutorManager  *LocalExecutorManager

	grpcConection *grpc.ClientConn
}

func NewAgentServer(option *AgentServerOption) *AgentServer {
	absoluteDir, err := filepath.Abs(util.CleanPath(*option.Dir))
	if err != nil {
		panic(err)
	}
	println("starting in", absoluteDir)
	option.Dir = &absoluteDir

	as := &AgentServer{
		Option:           option,
		Master:           *option.Master,
		storageBackend:   NewLocalDatasetShardsManager(*option.Dir, *option.Port),
		inMemoryChannels: NewLocalDatasetShardsManagerInMemory(),
		computeResource: &pb.ComputeResource{
			CpuCount: int32(*option.MaxExecutor),
			CpuLevel: int32(*option.CPULevel),
			MemoryMb: *option.MemoryMB,
		},
		allocatedResource:    &pb.ComputeResource{},
		localExecutorManager: newLocalExecutorsManager(),
	}

	go as.storageBackend.purgeExpiredEntries()
	go as.inMemoryChannels.purgeExpiredEntries()
	go as.localExecutorManager.purgeExpiredEntries()

	err = as.init()
	if err != nil {
		panic(err)
	}

	return as
}

func (r *AgentServer) init() (err error) {
	r.listener, err = net.Listen("tcp", *r.Option.Host+":"+strconv.Itoa(*r.Option.Port))

	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("AgentServer starts on", *r.Option.Host+":"+strconv.Itoa(*r.Option.Port))

	if *r.Option.CleanRestart {
		if fileInfos, err := ioutil.ReadDir(r.storageBackend.dir); err == nil {
			suffix := fmt.Sprintf("-%d.dat", *r.Option.Port)
			for _, fi := range fileInfos {
				name := fi.Name()
				if !fi.IsDir() && strings.HasSuffix(name, suffix) {
					// println("removing old dat file:", name)
					os.Remove(filepath.Join(r.storageBackend.dir, name))
				}
			}
		}
	}

	return
}

// Run starts the heartbeating to master and starts accepting requests.
func (as *AgentServer) Run() {

	go as.heartbeat()

	for {
		// Listen for an incoming connection.
		conn, err := as.listener.Accept()
		if err != nil {
			fmt.Println("Error accepting: ", err.Error())
			continue
		}
		// Handle connections in a new goroutine.
		as.wg.Add(1)
		go func() {
			defer as.wg.Done()
			defer conn.Close()
			if err = conn.SetDeadline(time.Time{}); err != nil {
				fmt.Printf("Failed to set timeout: %v\n", err)
			}
			if c, ok := conn.(*net.TCPConn); ok {
				c.SetKeepAlive(true)
			}
			as.handleRequest(conn)
		}()
	}
}

// Stop stops handling incoming requests and waits out all ongoing requests
func (r *AgentServer) Stop() {
	r.listener.Close()
	r.wg.Wait()
}

func (r *AgentServer) handleRequest(conn net.Conn) {

	data, err := util.ReadMessage(conn)

	if err != nil {
		log.Printf("Failed to read command:%v", err)
		return
	}

	newCmd := &msg.ControlMessage{}
	if err := proto.Unmarshal(data, newCmd); err != nil {
		log.Fatal("unmarshaling error: ", err)
	}
	reply := r.handleCommandConnection(conn, newCmd)
	if reply != nil {
		data, err := proto.Marshal(reply)
		if err != nil {
			log.Fatal("marshaling error: ", err)
		}
		conn.Write(data)
	}

}

func (as *AgentServer) handleCommandConnection(conn net.Conn,
	command *msg.ControlMessage) *msg.ControlMessage {
	reply := &msg.ControlMessage{}
	if command.GetReadRequest() != nil {
		if !command.GetIsOnDiskIO() {
			as.handleInMemoryReadConnection(conn, *command.ReadRequest.ReaderName, *command.ReadRequest.ChannelName)
		} else {
			as.handleReadConnection(conn, *command.ReadRequest.ReaderName, *command.ReadRequest.ChannelName)
		}
		return nil
	}
	if command.GetWriteRequest() != nil {
		if !command.GetIsOnDiskIO() {
			as.handleLocalInMemoryWriteConnection(conn, *command.WriteRequest.WriterName, *command.WriteRequest.ChannelName, int(command.GetWriteRequest().GetReaderCount()))
		} else {
			as.handleLocalWriteConnection(conn, *command.WriteRequest.WriterName, *command.WriteRequest.ChannelName, int(command.GetWriteRequest().GetReaderCount()))
		}
		return nil
	}
	if command.GetStartRequest() != nil {
		// println("start from", *command.StartRequest.Host)
		if *command.StartRequest.Host == "" {
			remoteAddress := conn.RemoteAddr().String()
			// println("remote address is", remoteAddress)
			host := remoteAddress[:strings.LastIndex(remoteAddress, ":")]
			command.StartRequest.Host = &host
		}
		reply.StartResponse = as.handleStart(conn, command.StartRequest)
		// return nil to avoid writing the response to the connection.
		// Currently the connection is used for reading outputs
		return nil
	}
	if command.GetDeleteDatasetShardRequest() != nil {
		reply.DeleteDatasetShardResponse = as.handleDeleteDatasetShard(conn, command.DeleteDatasetShardRequest)
	} else if command.GetGetStatusRequest() != nil {
		reply.GetStatusResponse = as.handleGetStatusRequest(command.GetGetStatusRequest())
	} else if command.GetStopRequest() != nil {
		reply.StopResponse = as.handleStopRequest(command.GetStopRequest())
	} else if command.GetLocalStatusReportRequest() != nil {
		reply.LocalStatusReportResponse = as.handleLocalStatusReportRequest(command.GetLocalStatusReportRequest())
	}
	return reply
}
