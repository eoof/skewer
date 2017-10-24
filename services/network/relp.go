package network

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/cornelk/hashmap"
	"github.com/oklog/ulid"
	"github.com/pquerna/ffjson/ffjson"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"golang.org/x/text/encoding"
	sarama "gopkg.in/Shopify/sarama.v1"

	"github.com/inconshreveable/log15"
	"github.com/stephane-martin/skewer/conf"
	"github.com/stephane-martin/skewer/javascript"
	"github.com/stephane-martin/skewer/model"
	"github.com/stephane-martin/skewer/services/base"
	"github.com/stephane-martin/skewer/services/errors"
	"github.com/stephane-martin/skewer/sys/binder"
	"github.com/stephane-martin/skewer/sys/capabilities"
	"github.com/stephane-martin/skewer/utils"
	"github.com/stephane-martin/skewer/utils/queue"
)

var tr = true
var fa = false

type RelpServerStatus int

const (
	Stopped RelpServerStatus = iota
	Started
	FinalStopped
	Waiting
)

type ackForwarder struct {
	succ hashmap.HashMap
	fail hashmap.HashMap
	comm hashmap.HashMap
	next uintptr
}

func newAckForwarder() *ackForwarder {
	return &ackForwarder{}
}

func (f *ackForwarder) Received(connID uintptr, txnr int) {
	if ptr, ok := f.comm.GetUintKey(connID); ok && ptr != nil {
		(*hashmap.HashMap)(ptr).Set(uintptr(txnr), unsafe.Pointer(&tr))
	}
}

func (f *ackForwarder) Committed(connID uintptr, txnr int) {
	if ptr, ok := f.comm.GetUintKey(connID); ok && ptr != nil {
		h := (*hashmap.HashMap)(ptr)
		h.Set(uintptr(txnr), unsafe.Pointer(&fa))
		h.Del(uintptr(txnr))
	}
}

func (f *ackForwarder) NextToCommit(connID uintptr) int {
	if ptr, ok := f.comm.GetUintKey(connID); ok && ptr != nil {
		h := (*hashmap.HashMap)(ptr)
		if h.Len() == 0 {
			return -1
		}
		var minimum int = -1
		for kv := range h.Iter() {
			if kv.Value != nil && *(*bool)(kv.Value) {
				if minimum == -1 {
					minimum = int(kv.Key.(uintptr))
				} else if int(kv.Key.(uintptr)) < minimum {
					minimum = int(kv.Key.(uintptr))
				}
			}
		}
		return minimum
	}
	return -1
}

func (f *ackForwarder) ForwardSucc(connID uintptr, txnr int) {
	if ptr, ok := f.succ.GetUintKey(connID); ok && ptr != nil {
		(*queue.IntQueue)(ptr).Put(txnr)
	}
}

func (f *ackForwarder) ForwardFail(connID uintptr, txnr int) {
	if ptr, ok := f.fail.GetUintKey(connID); ok && ptr != nil {
		(*queue.IntQueue)(ptr).Put(txnr)
	}
}

func (f *ackForwarder) AddConn() uintptr {
	connID := atomic.AddUintptr(&f.next, 1)
	f.succ.Set(connID, unsafe.Pointer(queue.NewIntQueue()))
	f.fail.Set(connID, unsafe.Pointer(queue.NewIntQueue()))
	f.comm.Set(connID, unsafe.Pointer(&hashmap.HashMap{}))
	return connID
}

func (f *ackForwarder) RemoveConn(connID uintptr) {
	if ptr, ok := f.succ.GetUintKey(connID); ok && ptr != nil {
		(*queue.IntQueue)(ptr).Dispose()
		f.succ.Set(connID, nil)
		f.succ.Del(connID)
	}
	if ptr, ok := f.fail.GetUintKey(connID); ok && ptr != nil {
		(*queue.IntQueue)(ptr).Dispose()
		f.fail.Set(connID, nil)
		f.fail.Del(connID)
	}
	if ptr, ok := f.comm.GetUintKey(connID); ok && ptr != nil {
		f.fail.Set(connID, nil)
		f.comm.Del(connID)
	}
}

func (f *ackForwarder) RemoveAll() {
	for kv := range f.succ.Iter() {
		f.RemoveConn(kv.Key.(uintptr))
	}
	for kv := range f.fail.Iter() {
		f.RemoveConn(kv.Key.(uintptr))
	}
}

func (f *ackForwarder) GetSucc(connID uintptr) int {
	if ptr, ok := f.succ.GetUintKey(connID); ok && ptr != nil {
		txnr, err := (*queue.IntQueue)(ptr).Get()
		if err != nil {
			return -1
		}
		return txnr
	}
	return -1
}

func (f *ackForwarder) GetFail(connID uintptr) int {
	if ptr, ok := f.fail.GetUintKey(connID); ok && ptr != nil {
		txnr, err := (*queue.IntQueue)(ptr).Get()
		if err != nil {
			return -1
		}
		return txnr
	}
	return -1
}

func (f *ackForwarder) Wait(connID uintptr) bool {
	ptrsucc, ok := f.succ.GetUintKey(connID)
	if !ok || ptrsucc == nil {
		return false
	}
	ptrfail, ok := f.fail.GetUintKey(connID)
	if !ok || ptrfail == nil {
		return false
	}
	return queue.WaitOne((*queue.IntQueue)(ptrsucc), (*queue.IntQueue)(ptrfail))
}

type meta struct {
	Txnr   int
	ConnID uintptr
}

type relpMetrics struct {
	IncomingMsgsCounter         *prometheus.CounterVec
	ClientConnectionCounter     *prometheus.CounterVec
	ParsingErrorCounter         *prometheus.CounterVec
	RelpAnswersCounter          *prometheus.CounterVec
	RelpProtocolErrorsCounter   *prometheus.CounterVec
	KafkaConnectionErrorCounter prometheus.Counter
	KafkaAckNackCounter         *prometheus.CounterVec
	MessageFilteringCounter     *prometheus.CounterVec
}

func NewRelpMetrics() *relpMetrics {
	m := &relpMetrics{}
	m.IncomingMsgsCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skw_incoming_messages_total",
			Help: "total number of messages that were received",
		},
		[]string{"protocol", "client", "port", "path"},
	)

	m.ClientConnectionCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skw_client_connections_total",
			Help: "total number of client connections",
		},
		[]string{"protocol", "client", "port", "path"},
	)

	m.ParsingErrorCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skw_parsing_errors_total",
			Help: "total number of times there was a parsing error",
		},
		[]string{"protocol", "client", "parser_name"},
	)

	m.RelpAnswersCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skw_relp_answers_total",
			Help: "number of RELP rsp answers",
		},
		[]string{"status", "client"},
	)

	m.RelpProtocolErrorsCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skw_relp_protocol_errors_total",
			Help: "Number of RELP protocol errors",
		},
		[]string{"client"},
	)

	m.KafkaConnectionErrorCounter = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "skw_relp_kafka_connection_errors_total",
			Help: "number of kafka connection errors",
		},
	)

	m.KafkaAckNackCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skw_relp_kafka_ack_total",
			Help: "number of kafka acknowledgments",
		},
		[]string{"status", "topic"},
	)

	m.MessageFilteringCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skw_relp_messages_filtering_total",
			Help: "number of filtered messages by status",
		},
		[]string{"status", "client"},
	)
	return m
}

type RelpService struct {
	impl      *RelpServiceImpl
	QueueSize uint64
	logger    log15.Logger
	reporter  *base.Reporter
	direct    bool
	b         *binder.BinderClient
	sc        []conf.SyslogConfig
	pc        []conf.ParserConfig
	kc        conf.KafkaDestConfig
	wg        sync.WaitGroup
	gen       chan ulid.ULID
}

func NewRelpService(r *base.Reporter, gen chan ulid.ULID, b *binder.BinderClient, l log15.Logger) *RelpService {
	s := &RelpService{b: b, logger: l, reporter: r, direct: true, gen: gen}
	s.impl = NewRelpServiceImpl(s.direct, gen, r, s.b, s.logger)
	return s
}

func (s *RelpService) Gather() ([]*dto.MetricFamily, error) {
	return s.impl.registry.Gather()
}

func (s *RelpService) Start(test bool) (infos []model.ListenerInfo, err error) {
	// the Relp service manages registration in Consul by itself and
	// therefore does not report infos
	if capabilities.CapabilitiesSupported {
		s.logger.Debug("Capabilities", "caps", capabilities.GetCaps())
	}
	infos = []model.ListenerInfo{}
	s.impl = NewRelpServiceImpl(s.direct, s.gen, s.reporter, s.b, s.logger)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			state := <-s.impl.StatusChan
			switch state {
			case FinalStopped:
				s.impl.Logger.Debug("The RELP service has been definitely halted")
				s.reporter.Report([]model.ListenerInfo{})
				return

			case Stopped:
				s.impl.Logger.Debug("The RELP service is stopped")
				s.impl.SetConf(s.sc, s.pc, s.kc, s.QueueSize)
				infos, err := s.impl.Start(test)
				if err == nil {
					s.reporter.Report(infos)
				} else {
					s.reporter.Report([]model.ListenerInfo{})
					s.impl.Logger.Warn("The RELP service has failed to start", "error", err)
					s.impl.StopAndWait()
				}

			case Waiting:
				s.impl.Logger.Debug("RELP waiting")
				go func() {
					time.Sleep(time.Duration(30) * time.Second)
					s.impl.EndWait()
				}()

			case Started:
				s.impl.Logger.Debug("The RELP service has been started")
			}
		}
	}()

	s.impl.StatusChan <- Stopped // trigger the RELP service to start
	return
}

func (s *RelpService) Shutdown() {
	s.Stop()
}

func (s *RelpService) Stop() {
	s.impl.FinalStop()
	s.wg.Wait()
}

func (s *RelpService) SetConf(sc []conf.SyslogConfig, pc []conf.ParserConfig, kc conf.KafkaDestConfig, direct bool, queueSize uint64) {
	s.sc = sc
	s.pc = pc
	s.kc = kc
	s.direct = direct
	s.QueueSize = queueSize
}

type RelpServiceImpl struct {
	StreamingService
	kafkaConf           conf.KafkaDestConfig
	status              RelpServerStatus
	StatusChan          chan RelpServerStatus
	producer            sarama.AsyncProducer
	test                bool
	metrics             *relpMetrics
	registry            *prometheus.Registry
	reporter            *base.Reporter
	direct              bool
	gen                 chan ulid.ULID
	rawMessagesQueue    *queue.RawTcpRing
	parsedMessagesQueue *queue.MessageQueue
	parsewg             sync.WaitGroup
	configs             map[ulid.ULID]conf.SyslogConfig
	forwarder           *ackForwarder
}

func NewRelpServiceImpl(direct bool, gen chan ulid.ULID, reporter *base.Reporter, b *binder.BinderClient, logger log15.Logger) *RelpServiceImpl {
	s := RelpServiceImpl{
		status:    Stopped,
		metrics:   NewRelpMetrics(),
		registry:  prometheus.NewRegistry(),
		reporter:  reporter,
		direct:    direct,
		gen:       gen,
		configs:   map[ulid.ULID]conf.SyslogConfig{},
		forwarder: newAckForwarder(),
	}
	s.StreamingService.init()
	s.registry.MustRegister(
		s.metrics.ClientConnectionCounter,
		s.metrics.IncomingMsgsCounter,
		s.metrics.KafkaAckNackCounter,
		s.metrics.KafkaConnectionErrorCounter,
		s.metrics.MessageFilteringCounter,
		s.metrics.ParsingErrorCounter,
		s.metrics.RelpAnswersCounter,
		s.metrics.RelpProtocolErrorsCounter,
	)
	s.StreamingService.BaseService.Logger = logger.New("class", "RelpServer")
	s.StreamingService.BaseService.Binder = b
	s.StreamingService.BaseService.Protocol = "relp"
	s.StreamingService.handler = RelpHandler{Server: &s}
	s.StatusChan = make(chan RelpServerStatus, 10)
	return &s
}

func (s *RelpServiceImpl) Start(test bool) ([]model.ListenerInfo, error) {
	s.LockStatus()
	defer s.UnlockStatus()
	if s.status == FinalStopped {
		return nil, errors.ServerDefinitelyStopped
	}
	if s.status != Stopped && s.status != Waiting {
		return nil, errors.ServerNotStopped
	}
	s.test = test

	infos := s.initTCPListeners()
	if len(infos) == 0 {
		s.Logger.Debug("RELP service not started: no listener")
		return infos, nil
	}

	s.producer = nil
	if !s.test && s.direct {
		var err error
		s.producer, err = s.kafkaConf.GetAsyncProducer()
		if err != nil {
			s.resetTCPListeners()
			return nil, err
		}
	}

	s.Logger.Info("Listening on RELP", "nb_services", len(infos))

	s.parsedMessagesQueue = queue.NewMessageQueue()
	s.rawMessagesQueue = queue.NewRawTcpRing(s.QueueSize)
	s.configs = map[ulid.ULID]conf.SyslogConfig{}

	for _, l := range s.UnixListeners {
		s.configs[l.Conf.ConfID] = l.Conf
	}
	for _, l := range s.TcpListeners {
		s.configs[l.Conf.ConfID] = l.Conf
	}

	if !s.test && s.direct {
		s.wg.Add(1)
		go s.push2kafka()
		s.wg.Add(1)
		go s.handleKafkaResponses()
	}
	cpus := runtime.NumCPU()
	for i := 0; i < cpus; i++ {
		s.parsewg.Add(1)
		go s.Parse()
	}

	s.status = Started
	s.StatusChan <- Started

	s.Listen()
	return infos, nil
}

func (s *RelpServiceImpl) Stop() {
	s.LockStatus()
	s.doStop(false, false)
	s.UnlockStatus()
}

func (s *RelpServiceImpl) FinalStop() {
	s.LockStatus()
	s.doStop(true, false)
	s.UnlockStatus()
}

func (s *RelpServiceImpl) StopAndWait() {
	s.LockStatus()
	s.doStop(false, true)
	s.UnlockStatus()
}

func (s *RelpServiceImpl) EndWait() {
	s.LockStatus()
	if s.status != Waiting {
		s.UnlockStatus()
		return
	}
	s.status = Stopped
	s.StatusChan <- Stopped
	s.UnlockStatus()
}

func (s *RelpServiceImpl) doStop(final bool, wait bool) {
	if final && (s.status == Waiting || s.status == Stopped || s.status == FinalStopped) {
		if s.status != FinalStopped {
			s.status = FinalStopped
			s.StatusChan <- FinalStopped
			close(s.StatusChan)
		}
		return
	}

	if s.status == Stopped || s.status == FinalStopped || s.status == Waiting {
		if s.status == Stopped && wait {
			s.status = Waiting
			s.StatusChan <- Waiting
		}
		return
	}

	s.resetTCPListeners() // makes the listeners stop
	// no more message will arrive in rawMessagesQueue
	if s.rawMessagesQueue != nil {
		s.rawMessagesQueue.Dispose()
	}
	// the parsers consume the rest of rawMessagesQueue, then they stop
	s.parsewg.Wait() // wait that the parsers have stopped
	if s.parsedMessagesQueue != nil {
		s.parsedMessagesQueue.Dispose()
	}

	// after the parsers have stopped, we can close the queues
	s.forwarder.RemoveAll()
	// wait that all goroutines have ended
	s.wg.Wait()

	if final {
		s.status = FinalStopped
		s.StatusChan <- FinalStopped
		close(s.StatusChan)
	} else if wait {
		s.status = Waiting
		s.StatusChan <- Waiting
	} else {
		s.status = Stopped
		s.StatusChan <- Stopped
	}
}

func (s *RelpServiceImpl) SetConf(sc []conf.SyslogConfig, pc []conf.ParserConfig, kc conf.KafkaDestConfig, queueSize uint64) {
	s.StreamingService.SetConf(sc, pc, queueSize, 132000)
	s.kafkaConf = kc
	s.BaseService.Pool = &sync.Pool{New: func() interface{} {
		return &model.RawTcpMessage{Message: make([]byte, 132000)}
	}}
}

func (s *RelpServiceImpl) Parse() {
	defer s.parsewg.Done()

	e := NewParsersEnv(s.ParserConfigs, s.Logger)

	var raw *model.RawTcpMessage
	var parser Parser
	var syslogMsg model.SyslogMessage
	var parsedMsg model.TcpUdpParsedMessage
	var err, f, nonf error
	var decoder *encoding.Decoder
	var logger log15.Logger

	for {
		raw, err = s.rawMessagesQueue.Get()
		if raw == nil || err != nil {
			break
		}

		logger = s.Logger.New(
			"protocol", s.Protocol,
			"client", raw.Client,
			"local_port", raw.LocalPort,
			"unix_socket_path", raw.UnixSocketPath,
			"format", raw.Format,
		)
		parser = e.GetParser(raw.Format)
		if parser == nil {
			logger.Crit("Unknown parser")
			s.Pool.Put(raw)
			return
		}
		decoder = utils.SelectDecoder(raw.Encoding)

		syslogMsg, err = parser.Parse(raw.Message[:raw.Size], decoder, raw.DontParseSD)
		if err != nil {
			s.metrics.ParsingErrorCounter.WithLabelValues(s.Protocol, raw.Client, raw.Format).Inc()
			logger.Warn("Parsing error", "message", raw.Message, "error", err)
			s.Pool.Put(raw)
			continue
		}
		if syslogMsg.Empty() {
			s.Pool.Put(raw)
			continue
		}

		parsedMsg = model.TcpUdpParsedMessage{
			Parsed: model.ParsedMessage{
				Fields:         syslogMsg,
				Client:         raw.Client,
				LocalPort:      raw.LocalPort,
				UnixSocketPath: raw.UnixSocketPath,
			},
			Txnr:   raw.Txnr,
			ConfId: raw.ConfID,
			ConnID: raw.ConnID,
		}
		s.Pool.Put(raw)

		if s.direct {
			// send message directly to kafka
			s.parsedMessagesQueue.Put(parsedMsg)
			continue
		}
		// else send message to the Store
		parsedMsg.Uid = <-s.gen
		f, nonf = s.reporter.Stash(parsedMsg)
		if f == nil && nonf == nil {
			s.forwarder.ForwardSucc(parsedMsg.ConnID, parsedMsg.Txnr)
		} else if f != nil {
			s.forwarder.ForwardFail(parsedMsg.ConnID, parsedMsg.Txnr)
			logger.Error("Fatal error pushing RELP message to the Store", "err", f)
			s.StopAndWait()
			return
		} else {
			s.forwarder.ForwardFail(parsedMsg.ConnID, parsedMsg.Txnr)
			logger.Warn("Non fatal error pushing RELP message to the Store", "err", nonf)
		}
	}

}

func (s *RelpServiceImpl) handleKafkaResponses() {
	var succ *sarama.ProducerMessage
	var fail *sarama.ProducerError
	var more, fatal bool
	kafkaSuccChan := s.producer.Successes()
	kafkaFailChan := s.producer.Errors()
	for {
		if kafkaSuccChan == nil && kafkaFailChan == nil {
			return
		}
		select {
		case succ, more = <-kafkaSuccChan:
			if more {
				metad := succ.Metadata.(meta)
				s.forwarder.ForwardSucc(metad.ConnID, metad.Txnr)
				s.metrics.KafkaAckNackCounter.WithLabelValues("ack", succ.Topic).Inc()
			} else {
				kafkaSuccChan = nil
			}
		case fail, more = <-kafkaFailChan:
			if more {
				metad := fail.Msg.Metadata.(meta)
				s.forwarder.ForwardFail(metad.ConnID, metad.Txnr)
				s.metrics.KafkaAckNackCounter.WithLabelValues("nack", fail.Msg.Topic).Inc()
				s.Logger.Info("NACK from Kafka", "error", fail.Error(), "txnr", metad.Txnr, "topic", fail.Msg.Topic)
				fatal = model.IsFatalKafkaError(fail.Err)
			} else {
				kafkaFailChan = nil
			}

		}

		if fatal {
			s.StopAndWait()
			return
		}

	}

}

func (s *RelpServiceImpl) handleResponses(conn net.Conn, connID uintptr, client string, logger log15.Logger) {
	defer s.wg.Done()

	successes := map[int]bool{}
	failures := map[int]bool{}

	immediateSuccess := func(txnr int) {
		answer := fmt.Sprintf("%d rsp 6 200 OK\n", txnr)
		conn.Write([]byte(answer))
		s.metrics.RelpAnswersCounter.WithLabelValues("200", client).Inc()
	}

	immediateFailure := func(txnr int) {
		answer := fmt.Sprintf("%d rsp 6 500 KO\n", txnr)
		conn.Write([]byte(answer))
		s.metrics.RelpAnswersCounter.WithLabelValues("500", client).Inc()
	}

	for s.forwarder.Wait(connID) {
		currentTxnr := s.forwarder.GetSucc(connID)
		if currentTxnr != -1 {
			successes[currentTxnr] = true
		} else {
			currentTxnr = s.forwarder.GetFail(connID)
			if currentTxnr != -1 {
				failures[currentTxnr] = true
			}
		}

		// rsyslog expects the ACK/txnr correctly and monotonously ordered
		// so we need a bit of cooking to ensure that
	Cooking:
		for {
			next := s.forwarder.NextToCommit(connID)
			if next == -1 {
				break Cooking
			}
			if successes[next] {
				immediateSuccess(next)
				delete(successes, next)
				s.forwarder.Committed(connID, next)
			} else if failures[next] {
				immediateFailure(next)
				delete(failures, next)
				s.forwarder.Committed(connID, next)
			} else {
				break Cooking
			}
		}
	}
}

func (s *RelpServiceImpl) push2kafka() {
	defer func() {
		s.producer.AsyncClose()
		s.wg.Done()
	}()
	envs := map[ulid.ULID]*javascript.Environment{}
	var e *javascript.Environment
	var haveEnv bool
	var message *model.TcpUdpParsedMessage
	var topic string
	var partitionKey string
	var partitionNumber int32
	var errs []error
	var err error
	var logger log15.Logger
	var filterResult javascript.FilterResult
	var kafkaMsg *sarama.ProducerMessage
	var serialized []byte
	var reported time.Time
	var config conf.SyslogConfig

ForParsedChan:
	for s.parsedMessagesQueue.Wait(0) {
		message, err = s.parsedMessagesQueue.Get()
		if err != nil {
			// should not happen
			s.Logger.Error("Fatal error getting messages from the parsed messages queue", "error", err)
			s.StopAndWait()
			return
		}
		if message == nil {
			// should not happen
			continue ForParsedChan
		}
		logger = s.Logger.New("client", message.Parsed.Client, "port", message.Parsed.LocalPort, "path", message.Parsed.UnixSocketPath)
		e, haveEnv = envs[message.ConfId]
		if !haveEnv {
			config, haveEnv = s.configs[message.ConfId]
			if !haveEnv {
				s.Logger.Warn("Could not find the configuration for a message", "confId", message.ConfId, "txnr", message.Txnr)
				continue ForParsedChan
			}
			envs[message.ConfId] = javascript.NewFilterEnvironment(
				config.FilterFunc,
				config.TopicFunc,
				config.TopicTmpl,
				config.PartitionFunc,
				config.PartitionTmpl,
				config.PartitionNumberFunc,
				s.Logger,
			)
			e = envs[message.ConfId]
		}

		topic, errs = e.Topic(message.Parsed.Fields)
		for _, err = range errs {
			logger.Info("Error calculating topic", "error", err, "txnr", message.Txnr)
		}
		if len(topic) == 0 {
			logger.Warn("Topic or PartitionKey could not be calculated", "txnr", message.Txnr)
			s.forwarder.ForwardFail(message.ConnID, message.Txnr)
			continue ForParsedChan
		}
		partitionKey, errs = e.PartitionKey(message.Parsed.Fields)
		for _, err = range errs {
			logger.Info("Error calculating the partition key", "error", err, "txnr", message.Txnr)
		}
		partitionNumber, errs = e.PartitionNumber(message.Parsed.Fields)
		for _, err = range errs {
			logger.Info("Error calculating the partition number", "error", err, "txnr", message.Txnr)
		}

		filterResult, err = e.FilterMessage(&message.Parsed.Fields)

		switch filterResult {
		case javascript.DROPPED:
			s.forwarder.ForwardFail(message.ConnID, message.Txnr)
			s.metrics.MessageFilteringCounter.WithLabelValues("dropped", message.Parsed.Client).Inc()
			continue ForParsedChan
		case javascript.REJECTED:
			s.forwarder.ForwardFail(message.ConnID, message.Txnr)
			s.metrics.MessageFilteringCounter.WithLabelValues("rejected", message.Parsed.Client).Inc()
			continue ForParsedChan
		case javascript.PASS:
			s.metrics.MessageFilteringCounter.WithLabelValues("passing", message.Parsed.Client).Inc()
		default:
			s.forwarder.ForwardFail(message.ConnID, message.Txnr)
			s.metrics.MessageFilteringCounter.WithLabelValues("unknown", message.Parsed.Client).Inc()
			logger.Warn("Error happened processing message", "txnr", message.Txnr, "error", err)
			continue ForParsedChan
		}

		reported = time.Unix(0, message.Parsed.Fields.TimeReportedNum).UTC()
		message.Parsed.Fields.TimeGenerated = time.Unix(0, message.Parsed.Fields.TimeGeneratedNum).UTC().Format(time.RFC3339Nano)
		message.Parsed.Fields.TimeReported = reported.Format(time.RFC3339Nano)

		serialized, err = ffjson.Marshal(&message.Parsed)

		if err != nil {
			logger.Warn("Error generating Kafka message", "error", err, "txnr", message.Txnr)
			s.forwarder.ForwardFail(message.ConnID, message.Txnr)
			continue ForParsedChan
		}

		kafkaMsg = &sarama.ProducerMessage{
			Key:       sarama.StringEncoder(partitionKey),
			Partition: partitionNumber,
			Value:     sarama.ByteEncoder(serialized),
			Topic:     topic,
			Timestamp: reported,
			Metadata:  meta{Txnr: message.Txnr, ConnID: message.ConnID},
		}

		if s.test {
			// "fake" send messages to kafka
			fmt.Fprintf(os.Stderr, "pkey: '%s' topic:'%s' txnr:'%d'\n", partitionKey, topic, message.Txnr)
			fmt.Fprintln(os.Stderr, string(serialized))
			fmt.Fprintln(os.Stderr)
			s.forwarder.ForwardSucc(message.ConnID, message.Txnr)
		} else {
			// send messages to Kafka
			s.producer.Input() <- kafkaMsg
		}
		ffjson.Pool(serialized)
	}

}

type RelpHandler struct {
	Server *RelpServiceImpl
}

func (h RelpHandler) HandleConnection(conn net.Conn, config conf.SyslogConfig) {
	// http://www.rsyslog.com/doc/relp.html
	s := h.Server
	s.AddConnection(conn)
	connID := s.forwarder.AddConn()

	defer func() {
		s.forwarder.RemoveConn(connID)
		s.RemoveConnection(conn)
		s.wg.Done()
	}()

	var relpIsOpen bool

	client := ""
	path := ""
	remote := conn.RemoteAddr()

	var localPort int
	if remote == nil {
		client = "localhost"
		localPort = 0
		path = conn.LocalAddr().String()
	} else {
		client = strings.Split(remote.String(), ":")[0]
		local := conn.LocalAddr()
		if local != nil {
			s := strings.Split(local.String(), ":")
			localPort, _ = strconv.Atoi(s[len(s)-1])
		}
	}
	client = strings.TrimSpace(client)
	path = strings.TrimSpace(path)
	localPortStr := strconv.FormatInt(int64(localPort), 10)

	logger := s.Logger.New("protocol", s.Protocol, "client", client, "local_port", localPort, "unix_socket_path", path, "format", config.Format)
	logger.Info("New client connection")
	s.metrics.ClientConnectionCounter.WithLabelValues(s.Protocol, client, localPortStr, path).Inc()

	s.wg.Add(1)
	go s.handleResponses(conn, connID, client, logger)

	/*
		var producer sarama.AsyncProducer
		if s.direct && !s.test {
			producer, err = s.kafkaConf.GetAsyncProducer()
			if err != nil {
				s.metrics.KafkaConnectionErrorCounter.Inc()
				logger.Warn("Can't get a kafka producer. Aborting handleConn.")
				return
			}
			// AsyncClose will eventually terminate the goroutine just below
			defer producer.AsyncClose()
		}
	*/

	timeout := config.Timeout
	if timeout > 0 {
		conn.SetReadDeadline(time.Now().Add(timeout))
	}
	scanner := bufio.NewScanner(conn)
	scanner.Split(RelpSplit)
	scanner.Buffer(make([]byte, 0, 132000), 132000)
	var rawmsg *model.RawTcpMessage
	var previous int = -1

Loop:
	for scanner.Scan() {
		if timeout > 0 {
			conn.SetReadDeadline(time.Now().Add(timeout))
		}
		line := scanner.Bytes()
		splits := bytes.SplitN(line, []byte(" "), 4)
		txnr, _ := strconv.Atoi(string(splits[0]))
		if txnr <= previous {
			logger.Warn("TXNR did not increase")
			s.metrics.RelpProtocolErrorsCounter.WithLabelValues(client).Inc()
			return
		}
		previous = txnr
		command := string(splits[1])
		datalen, _ := strconv.Atoi(string(splits[2]))
		data := []byte{}
		if datalen != 0 {
			if len(splits) == 4 {
				data = bytes.Trim(splits[3], " \r\n")
			} else {
				logger.Warn("datalen is non-null, but no data is provided")
				s.metrics.RelpProtocolErrorsCounter.WithLabelValues(client).Inc()
				return
			}
		}
		switch command {
		case "open":
			if relpIsOpen {
				logger.Warn("Received open command twice")
				s.metrics.RelpProtocolErrorsCounter.WithLabelValues(client).Inc()
				return
			}
			answer := fmt.Sprintf("%d rsp %d 200 OK\n%s\n", txnr, len(data)+7, string(data))
			conn.Write([]byte(answer))
			relpIsOpen = true
			logger.Info("Received 'open' command")
		case "close":
			if !relpIsOpen {
				logger.Warn("Received close command before open")
				s.metrics.RelpProtocolErrorsCounter.WithLabelValues(client).Inc()
				return
			}
			answer := fmt.Sprintf("%d rsp 0\n0 serverclose 0\n", txnr)
			conn.Write([]byte(answer))
			conn.Close()
			relpIsOpen = false
			logger.Info("Received 'close' command")
		case "syslog":
			if !relpIsOpen {
				logger.Warn("Received syslog command before open")
				s.metrics.RelpProtocolErrorsCounter.WithLabelValues(client).Inc()
				return
			}
			s.forwarder.Received(connID, txnr)
			if len(data) == 0 {
				s.forwarder.ForwardSucc(connID, txnr)
				continue Loop
			}
			rawmsg = s.Pool.Get().(*model.RawTcpMessage)
			rawmsg.Size = len(data)
			rawmsg.Txnr = txnr
			rawmsg.Client = client
			rawmsg.LocalPort = localPort
			rawmsg.UnixSocketPath = path
			rawmsg.ConfID = config.ConfID
			rawmsg.DontParseSD = config.DontParseSD
			rawmsg.Encoding = config.Encoding
			rawmsg.Format = config.Format
			rawmsg.ConnID = connID
			copy(rawmsg.Message, data)
			s.metrics.IncomingMsgsCounter.WithLabelValues(s.Protocol, client, localPortStr, path).Inc()
			s.rawMessagesQueue.Put(rawmsg)
		default:
			logger.Warn("Unknown RELP command", "command", command)
			s.metrics.RelpProtocolErrorsCounter.WithLabelValues(client).Inc()
			return
		}
	}
	logger.Info("Scanning the RELP stream has ended", "error", scanner.Err())
}

func splitSpaceOrLF(r rune) bool {
	return r == ' ' || r == '\n' || r == '\r'
}

// RelpSplit is used to extract RELP lines from the incoming TCP stream
func RelpSplit(data []byte, atEOF bool) (int, []byte, error) {
	trimmedData := bytes.TrimLeft(data, " \r\n")
	if len(trimmedData) == 0 {
		return 0, nil, nil
	}
	splits := bytes.FieldsFunc(trimmedData, splitSpaceOrLF)
	l := len(splits)
	if l < 3 {
		// Request more data
		return 0, nil, nil
	}

	txnrStr := string(splits[0])
	command := string(splits[1])
	datalenStr := string(splits[2])
	tokenStr := txnrStr + " " + command + " " + datalenStr
	advance := len(data) - len(trimmedData) + len(tokenStr) + 1

	if l == 3 && (len(data) < advance) {
		// datalen field is not complete, request more data
		return 0, nil, nil
	}

	_, err := strconv.Atoi(txnrStr)
	if err != nil {
		return 0, nil, err
	}
	datalen, err := strconv.Atoi(datalenStr)
	if err != nil {
		return 0, nil, err
	}
	if datalen == 0 {
		return advance, []byte(tokenStr), nil
	}
	advance += datalen + 1
	if len(data) >= advance {
		token := bytes.Trim(data[:advance], " \r\n")
		return advance, token, nil
	}
	// Request more data
	return 0, nil, nil
}
