package cluster

import (
	"expvar"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"

	"fmt"
	"github.com/influxdata/influxdb/coordinator"
	"github.com/influxdata/influxdb/influxql"
	"github.com/influxdata/influxdb/services/meta"
	"github.com/influxdata/influxdb/tsdb"
	"github.com/zhexuany/influxdb-cluster/rpc"
	"github.com/zhexuany/influxdb-cluster/tlv"
)

// MaxMessageSize defines how large a message can be before we reject it
const MaxMessageSize = 1024 * 1024 * 1024 // 1GB

// MuxHeader is the header byte used in the TCP mux.
const MuxHeader = 2

// Service reprsents a cluster service
type Service struct {
	mu sync.RWMutex

	wg      sync.WaitGroup
	closing chan struct{}

	Listener net.Listener

	MetaClient interface {
		ShardOwner(shardID uint64) (string, string, meta.ShardGroupInfos)
	}

	TSDBStore coordinator.TSDBStore

	ShardIteratorCreator coordinator.ShardIteratorCreator

	Logger  *log.Logger
	statMap *expvar.Map
}

// NewService returns a new instance of Service.
func NewService(c Config) *Service {
	return &Service{
		closing: make(chan struct{}),
		Logger:  log.New(os.Stderr, "[cluster] ", log.LstdFlags),
	}
}

// Open opens the network listener and begins serving requests
func (s *Service) Open() error {
	s.Logger.Println("Starting cluster service")
	s.wg.Add(1)
	go s.serve()

	return nil
}

// SetLogOutput sets the internal logger to the logger passed in
func (s *Service) SetLogOutput(l *log.Logger) {
	s.Logger = l
}

// serve accepts connections from the listener and handles them
func (s *Service) serve() {
	defer s.wg.Done()

	for {
		// Check if the service is shutting down
		select {
		case <-s.closing:
			return
		default:
		}

		// Accept the next connection
		conn, err := s.Listener.Accept()
		if err != nil {
			if strings.Contains(err.Error(), "connection closed") {
				s.Logger.Printf("cluster service accept error: %s", err)
				return
			}
			s.Logger.Printf("accept error: %s", err)
			continue
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

// Close close this service
func (s *Service) Close() error {
	if s.Listener != nil {
		s.Listener.Close()
	}

	close(s.closing)
	s.wg.Wait()

	return nil
}

func (s *Service) handleConn(conn net.Conn) {
	//Ensuring connection is closed when service is closed
	closing := make(chan struct{})
	defer close(closing)
	go func() {
		select {
		case <-closing:
		case <-s.closing:
		}
		conn.Close()
	}()

	s.Logger.Printf("accept remote connection from %v\n", conn.RemoteAddr())
	defer func() {
		s.Logger.Printf("close remote connection from %v\n", conn.RemoteAddr())
	}()
	for {
		// Read type-length-value.
		typ, err := tlv.ReadType(conn)
		if err != nil {
			if strings.HasSuffix(err.Error(), "EOF") {
				return
			}
			s.Logger.Printf("unable to read type: %s", err)
			return
		}

		// Delegate message processing by type.
		switch typ {
		case tlv.WriteShardRequestMessage:
			buf, err := tlv.ReadLV(conn)
			if err != nil {
				s.Logger.Printf("unable to read length-value: %s", err)
				return
			}

			err = s.processWriteShardRequest(buf)
			if err != nil {
				s.Logger.Printf("process write shard error: %s", err)
			}
			s.writeShardResponse(conn, err)
		case tlv.ExecuteStatementRequestMessage:
			buf, err := tlv.ReadLV(conn)
			if err != nil {
				s.Logger.Printf("unable to read length-value: %s", err)
				return
			}

			err = s.processExecuteStatementRequest(buf)
			if err != nil {
				s.Logger.Printf("process execute statement error: %s", err)
			}
			s.writeShardResponse(conn, err)
		case tlv.CreateIteratorRequestMessage:
			s.processCreateIteratorRequest(conn)
			return
		case tlv.FieldDimensionsRequestMessage:
			s.processFieldDimensionsRequest(conn)
			return
		// case seriesKeysRequestMessage:
		// s.processSeriesKeysRequest(conn)
		// return
		default:
			s.Logger.Printf("cluster service message type not found: %d", typ)
		}
	}

}

func (s *Service) executeStatement(stmt influxql.Statement, database string) error {
	switch t := stmt.(type) {
	case *influxql.DropDatabaseStatement:
		// return s.TSDBStore.DeleteDatabase(t.Name)
	case *influxql.DropMeasurementStatement:
		// return s.TSDBStore.DeleteMeasurement(database, t.Name)
	case *influxql.DropSeriesStatement:
		// return s.TSDBStore.DeleteSeries(database, t.Sources, t.Condition)
	case *influxql.DropRetentionPolicyStatement:
		// return s.TSDBStore.DeleteRetentionPolicy(database, t.Name)
	default:
		return fmt.Errorf("%q should not be executed across a cluster", stmt.String())
	}
	return nil
}
func (s *Service) processWriteShardRequest(buf []byte) error {
	// Build request
	var req rpc.WriteShardRequest
	if err := req.UnmarshalBinary(buf); err != nil {
		return err
	}

	points := req.Points()
	err := s.TSDBStore.WriteToShard(req.ShardID(), points)

	// We may have received a write for a shard that we don't have locally because the
	// sending node may have just created the shard (via the metastore) and the write
	// arrived before the local store could create the shard.  In this case, we need
	// to check the metastore to determine what database and retention policy this
	// shard should reside within.
	if err == tsdb.ErrShardNotFound {
		db, rp := req.Database(), req.RetentionPolicy()
		if db == "" || rp == "" {
			s.Logger.Printf("drop write request: shard=%d. no database or rentention policy received", req.ShardID())
			return nil
		}

		err = s.TSDBStore.CreateShard(req.Database(), req.RetentionPolicy(), req.ShardID(), true)
		if err != nil {
			return fmt.Errorf("create shard %d: %s", req.ShardID(), err)
		}

		err = s.TSDBStore.WriteToShard(req.ShardID(), points)
		if err != nil {
			return fmt.Errorf("write shard %d: %s", req.ShardID(), err)
		}
	}

	if err != nil {
		return fmt.Errorf("write shard %d: %s", req.ShardID(), err)
	}

	return nil
}

func (s *Service) writeShardResponse(conn net.Conn, err error) {
	// Build response.
	var resp rpc.WriteShardResponse
	if err != nil {
		resp.SetCode(1)
		resp.SetMessage(err.Error())
	} else {
		resp.SetCode(0)
	}

	// Marshal response to binary.
	buf, err := resp.MarshalBinary()
	if err != nil {
		s.Logger.Printf("error marshalling shard response: %s", err)
		return
	}

	// Write to connection.
	if err := tlv.WriteTLV(conn, tlv.WriteShardResponseMessage, buf); err != nil {
		s.Logger.Printf("write shard response error: %s", err)
	}
}

func readUntilEOF() {

}
func (s *Service) processCreateIteratorRequest(conn net.Conn) {
	defer conn.Close()

	var itr influxql.Iterator
	if err := func() error {
		// Parse request.
		var req rpc.CreateIteratorRequest
		if err := tlv.DecodeLV(conn, &req); err != nil {
			return err
		}

		// Collect iterator creators for each shard.
		ics := make([]influxql.IteratorCreator, 0, len(req.ShardIDs))
		// for _, shardID := range req.ShardIDs {
		// 	ic := s.ShardIteratorCreator.ShardIteratorCreator(shardID)
		// 	if ic == nil {
		// 		return nil
		// 	}
		// 	ics = append(ics, ic)
		// }

		// // Generate a single iterator from all shards.
		// i, err := influxql.IteratorCreators(ics).CreateIterator(req.Opt)
		// if err != nil {
		// 	return err
		// }
		// itr = i

		return nil
	}(); err != nil {
		itr.Close()
		s.Logger.Printf("error reading CreateIterator request: %s", err)
		// tlv.EncodeTLV(conn, tlv.CreateIteratorResponseMessage, &CreateIteratorResponse{Err: err})

		tlv.EncodeTLV(conn, tlv.CreateIteratorResponseMessage, nil)
		return
	}

	// Encode success response.
	if err := tlv.EncodeTLV(conn, tlv.CreateIteratorResponseMessage, nil); err != nil {
		s.Logger.Printf("error writing CreateIterator response: %s", err)
		return
	}

	// Exit if no iterator was produced.
	if itr == nil {
		return
	}

	// Stream iterator to connection.
	if err := influxql.NewIteratorEncoder(conn).EncodeIterator(itr); err != nil {
		s.Logger.Printf("error encoding CreateIterator iterator: %s", err)
		return
	}
}

func (s *Service) processFieldDimensionsRequest(conn net.Conn) {
	var fields, dimensions map[string]struct{}
	if err := func() error {
		// Parse request.
		var req rpc.FieldDimensionsRequest
		if err := tlv.DecodeLV(conn, &req); err != nil {
			return err
		}

		// Collect iterator creators for each shard.
		ics := make(influxql.Iterators, 0, len(req.ShardIDs))
		// for _, shardID := range req.ShardIDs {
		// 	ic := s.ShardIteratorCreator.ShardIteratorCreator(shardID)
		// 	if ic == nil {
		// 		return nil
		// 	}
		// 	// ics = append(ics, ic.CreateIterator(nil))
		// }

		// // Generate a single iterator from all shards.
		// i, _ := ics.Merge(nil)
		// f, d, err := influxql.FieldMapper.FieldDimensions(nil)
		// // f, d, err := influxql.IteratorCreators(ics).FieldDimensions(req.Sources)
		// if err != nil {
		// 	return err
		// }
		// fields, dimensions = f, d

		// return nil
	}(); err != nil {
		s.Logger.Printf("error reading FieldDimensions request: %s", err)
		tlv.EncodeTLV(conn, tlv.FieldDimensionsResponseMessage, nil)
		return
	}

	// Encode success response.
	if err := tlv.EncodeTLV(conn, tlv.FieldDimensionsResponseMessage, &rpc.FieldDimensionsResponse{
		Fields:     fields,
		Dimensions: dimensions,
	}); err != nil {
		s.Logger.Printf("error writing FieldDimensions response: %s", err)
		return
	}
}

func (s *Service) processJoinClusterRequest() {

}
func (s *Service) writeJoinClusterResponse() {

}
func (s *Service) importMetaData() {

}
func (s *Service) processLeaveClusterRequest() {

}
func (s *Service) writeLeaveClusterResponse() {

}
func (s *Service) processCreateShardSnapshotRequest() {

}
func (s *Service) processDeleteShardSnapshotRequest() {

}

// ReadTLV drains reader
func ReadTLV(r io.Reader) (byte, []byte, error) {
	typ, err := tlv.ReadType(r)
	if err != nil {
		return 0, nil, err
	}

	buf, err := tlv.ReadLV(r)
	if err != nil {
		return 0, nil, err
	}
	return typ, buf, err
}

func (s *Service) processExpandSourcesRequest() {

}
func (s *Service) processDownloadShardSnapshotRequest() {

}

func (s *Service) shardSnapshot() {

}
func (s *Service) deleteSnapshot() {

}
func (s *Service) downloadShardSnapshot() {

}
func (s *Service) processShardStatusRequest() {

}
func (s *Service) processShowQueriesRequest() {

}
func (s *Service) processKillQueryRequest() {

}
func (s *Service) processRestoreShard() {

}
func (s *Service) processShowMeasurements() {

}
func (s *Service) processShowTagValues() {

}

func (s *Service) processExecuteStatementRequest(buf []byte) error {
	return nil
}

// BufferedWriteCloser will
type BufferedWriteCloser struct {
}

// Close is actually closing this bufferedwriter
func (bfc *BufferedWriteCloser) Close() {

}
