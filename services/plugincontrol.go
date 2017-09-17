package services

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/inconshreveable/log15"
	"github.com/kardianos/osext"
	dto "github.com/prometheus/client_model/go"
	"github.com/stephane-martin/skewer/conf"
	"github.com/stephane-martin/skewer/consul"
	"github.com/stephane-martin/skewer/model"
	"github.com/stephane-martin/skewer/sys"
	"github.com/stephane-martin/skewer/utils"
)

// PluginController launches and controls the plugins services
type PluginController struct {
	typ NetworkServiceType

	conf conf.BaseConfig

	binderHandle int
	loggerHandle int
	pipe         *os.File
	logger       log15.Logger
	stasher      model.Stasher
	registry     *consul.Registry

	metricsChan chan []*dto.MetricFamily
	stdin       io.WriteCloser
	stdout      io.ReadCloser
	cmd         *exec.Cmd

	ShutdownChan chan struct{}
	StopChan     chan struct{}
	// ExitCode should be read only after ShutdownChan has been closed
	ExitCode  int
	stdinMu   *sync.Mutex
	startedMu *sync.Mutex
	createdMu *sync.Mutex
	started   bool
	created   bool
}

func NewPluginController(typ NetworkServiceType, stasher model.Stasher, r *consul.Registry, binderHandle int, loggerHandle int, l log15.Logger) *PluginController {
	s := &PluginController{
		typ:          typ,
		stasher:      stasher,
		registry:     r,
		binderHandle: binderHandle,
		loggerHandle: loggerHandle,
		logger:       l,
		stdinMu:      &sync.Mutex{},
		startedMu:    &sync.Mutex{},
		createdMu:    &sync.Mutex{},
	}
	s.metricsChan = make(chan []*dto.MetricFamily)
	s.ShutdownChan = make(chan struct{})
	return s
}

func (s *PluginController) W(header string, message []byte) (err error) {
	s.stdinMu.Lock()
	if s.stdin != nil {
		err = utils.W(s.stdin, header, message)
	} else {
		err = fmt.Errorf("stdin is nil")
	}
	s.stdinMu.Unlock()
	return err
}

func (s *PluginController) Gather() ([]*dto.MetricFamily, error) {
	select {
	case <-s.ShutdownChan:
		return []*dto.MetricFamily{}, nil
	default:
		s.startedMu.Lock()
		defer s.startedMu.Unlock()
		if s.started {
			if s.W("gathermetrics", utils.NOW) != nil {
				return []*dto.MetricFamily{}, nil
			}

			select {
			case <-time.After(2 * time.Second):
				return []*dto.MetricFamily{}, nil
			case metrics, more := <-s.metricsChan:
				if more {
					if metrics != nil {
						return metrics, nil
					} else {
						return []*dto.MetricFamily{}, nil
					}
				} else {
					return []*dto.MetricFamily{}, nil
				}
			}
		} else {
			return []*dto.MetricFamily{}, nil
		}
	}
}

func (s *PluginController) Stop() {
	// in case the plugin was in fact never created...
	s.createdMu.Lock()
	if !s.created {
		s.createdMu.Unlock()
		return
	}
	s.createdMu.Unlock()

	select {
	case <-s.ShutdownChan:
	case <-s.StopChan:
	default:
		err := s.W("stop", utils.NOW)
		if err != nil {
			s.logger.Warn("Error writing stop to plugin stdin", "error", err)
		} else {
			<-s.StopChan
		}
	}
}

func (s *PluginController) Shutdown(killTimeOut time.Duration) {
	// in case the plugin was in fact never created...
	s.createdMu.Lock()
	if !s.created {
		s.createdMu.Unlock()
		return
	}
	s.createdMu.Unlock()

	select {
	case <-s.ShutdownChan:
		// the plugin process is already dead
		<-s.StopChan
	default:
		// ask to shutdown
		err := s.W("shutdown", utils.NOW)
		if err != nil {
			s.logger.Warn("Error writing shutdown to plugin stdin. Kill brutally.", "error", err)
			killTimeOut = time.Second
		}

		// wait for plugin process termination
		if killTimeOut == 0 {
			<-s.ShutdownChan
			<-s.StopChan
		} else {
			select {
			case <-s.ShutdownChan:
				<-s.StopChan
			case <-time.After(killTimeOut):
				// after timeout kill the process
				s.logger.Warn("Plugin failed to shutdown before timeout")
				s.kill(false)
				<-s.ShutdownChan
				<-s.StopChan
			}
		}
	}

}

func (s *PluginController) SetConf(c conf.BaseConfig) {
	s.conf = c
}

func (s *PluginController) kill(misbevave bool) {
	if misbevave {
		s.logger.Crit("killing misbehaving plugin", "type", ReverseNetworkServiceMap[s.typ])
	}
	s.stdinMu.Lock()
	s.cmd.Process.Kill()
	s.stdinMu.Unlock()
}

type InfosAndError struct {
	infos []model.ListenerInfo
	err   error
}

func (s *PluginController) listen() chan InfosAndError {
	startErrorChan := make(chan InfosAndError)

	go func() {
		var once sync.Once
		initialized := false
		kill := false
		name := ReverseNetworkServiceMap[s.typ]
		normalStop := false

		defer func() {
			s.logger.Debug("Plugin controller is stopping", "type", name)
			once.Do(func() {
				startErrorChan <- InfosAndError{
					err:   fmt.Errorf("Unexpected end of plugin before it was initialized"),
					infos: nil,
				}
				close(startErrorChan)
			})
			s.createdMu.Lock()
			s.startedMu.Lock()
			s.started = false

			select {
			case <-s.ShutdownChan:
				// child process has already exited
				s.logger.Debug("Plugin child process has shut down", "type", name)
				s.created = false
			default:
				// child process is still alive, but we are in the defer(). why ?
				if kill {
					// the child misbehaved and deserved to be killed
					s.kill(true)
					<-s.ShutdownChan
					s.created = false
				} else if normalStop {
					s.logger.Debug("Plugin child process has stopped normally", "type", name)
				} else {
					// should not happen, we assume an anomaly
					s.kill(true)
					<-s.ShutdownChan
					s.created = false
				}
			}

			s.startedMu.Unlock()
			s.createdMu.Unlock()
			close(s.StopChan)
		}() // end of defer

		// read JSON encoded messages that the plugin is going to write on stdout
		scanner := bufio.NewScanner(s.stdout)
		scanner.Split(utils.PluginSplit)
		command := ""
		infos := []model.ListenerInfo{}

		for scanner.Scan() {
			parts := strings.SplitN(scanner.Text(), " ", 2)
			command = parts[0]
			switch command {
			case "syslog":
				// the plugin emitted a syslog message to be sent to the Store
				if len(parts) == 2 {
					if !initialized {
						msg := "Plugin sent a syslog message before being initialized"
						s.logger.Error(msg)
						once.Do(func() {
							startErrorChan <- InfosAndError{
								err:   fmt.Errorf(msg),
								infos: nil,
							}
							close(startErrorChan)
						})
						kill = true
						return
					} else {
						m := &model.TcpUdpParsedMessage{}
						_, err := m.UnmarshalMsg([]byte(parts[1]))
						if err == nil {
							s.stasher.Stash(m)
						} else {
							s.logger.Warn("Plugin sent a badly encoded log line", "error", err)
							kill = true
							return
						}
					}
				}
			case "started":
				if len(parts) == 2 {
					// fill infos about listening ports
					infos := []model.ListenerInfo{}
					err := json.Unmarshal([]byte(parts[1]), &infos)
					if err == nil {
						initialized = true
						once.Do(func() {
							startErrorChan <- InfosAndError{
								infos: infos,
								err:   nil,
							}
							close(startErrorChan)
						})
					} else {
						s.logger.Warn("Plugin sent a badly encoded JSON listener info", "error", err)
						once.Do(func() {
							startErrorChan <- InfosAndError{
								infos: nil,
								err:   err,
							}
							close(startErrorChan)
						})
						kill = true
						return
					}
				}
			case "infos":
				if len(parts) == 2 {
					newinfos := []model.ListenerInfo{}
					err := json.Unmarshal([]byte(parts[1]), &newinfos)
					if err == nil {
						s.logger.Info("reported infos", "infos", newinfos, "type", name)
						if s.registry != nil {
							if len(infos) > 0 {
								for _, info := range infos {
									s.registry.UnregisterTcpListener(info)
								}
							}
							if len(newinfos) > 0 {
								for _, info := range newinfos {
									s.registry.RegisterTcpListener(info)
								}
							}
							infos = newinfos
						}
					}
				}
			case "stopped":
				normalStop = true
				return
			case "shutdown":
				// plugin child is shutting down, eventually the scanner will return normally, we just wait for that
			case "starterror":
				if len(parts) == 2 {
					err := fmt.Errorf(parts[1])
					once.Do(func() {
						startErrorChan <- InfosAndError{
							infos: nil,
							err:   err,
						}
						close(startErrorChan)
					})
				}
			case "conferror":
				if len(parts) == 2 {
					err := fmt.Errorf(parts[1])
					once.Do(func() {
						startErrorChan <- InfosAndError{
							infos: nil,
							err:   err,
						}
						close(startErrorChan)
					})
				}
			case "nolistenererror":
				err := fmt.Errorf("No listener")
				once.Do(func() {
					startErrorChan <- InfosAndError{
						infos: nil,
						err:   err,
					}
					close(startErrorChan)
				})
			case "metrics":
				if len(parts) == 2 {
					families := []*dto.MetricFamily{}
					err := json.Unmarshal([]byte(parts[1]), &families)
					if err == nil {
						s.metricsChan <- families
					} else {
						s.logger.Error("Plugin returned invalid metrics")
						close(s.metricsChan)
						kill = true
						return
					}
				} else {
					s.logger.Error("Plugin returned empty metrics")
					close(s.metricsChan)
					kill = true
					return
				}
			default:
				err := fmt.Errorf("Unexpected message from plugin")
				s.logger.Error("Unexpected message from plugin", "command", command)
				once.Do(func() {
					startErrorChan <- InfosAndError{
						infos: nil,
						err:   err,
					}
					close(startErrorChan)
				})
				kill = true
				return
			}
		}
		err := scanner.Err()
		if err == nil {
			// 'scanner' has returned without error.
			// It means that the plugin child stdout is EOF = closed.
			// So we know that the plugin child has exited
			// Let's wait that the shutdown channel has been closed before executing the defer()
			<-s.ShutdownChan
			return
		} else {
			// plugin has sent an invalid message that could not be interpreted by scanner
			once.Do(func() {
				startErrorChan <- InfosAndError{
					infos: nil,
					err:   err,
				}
				close(startErrorChan)
			})
			s.logger.Error("Plugin scanner error", "error", err)
			kill = true
			return
		}

	}()
	return startErrorChan
}

func (s *PluginController) Start() ([]model.ListenerInfo, error) {
	s.createdMu.Lock()
	s.startedMu.Lock()
	name := ReverseNetworkServiceMap[s.typ]
	if !s.created {
		s.startedMu.Unlock()
		s.createdMu.Unlock()
		return nil, fmt.Errorf("Can not start, plugin '%s' has not been created", name)
	}
	if s.started {
		s.startedMu.Unlock()
		s.createdMu.Unlock()
		return nil, fmt.Errorf("Plugin already started: %s", name)
	}
	s.StopChan = make(chan struct{})

	cb, _ := json.Marshal(s.conf)

	rerr := s.W("conf", cb)
	if rerr == nil {
		rerr = s.W("start", utils.NOW)
	}
	infos := []model.ListenerInfo{}
	if rerr == nil {
		select {
		case infoserr := <-s.listen():
			rerr = infoserr.err
			infos = infoserr.infos
		case <-time.After(3 * time.Second):
			rerr = fmt.Errorf("Plugin failed to start before timeout")
		}
	}

	if rerr == nil {
		s.started = true
		s.startedMu.Unlock()
		s.createdMu.Unlock()
		return infos, nil
	} else {
		s.startedMu.Unlock()
		s.createdMu.Unlock()
		s.Shutdown(3 * time.Second)
		return nil, rerr
	}
}

func setupCmd(name string, binderHandle int, loggerHandle int, messagePipe *os.File, test bool) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
	exe, err := osext.Executable()
	if err != nil {
		return nil, nil, nil, err
	}
	envs := []string{"PATH=/bin:/usr/bin"}
	files := []*os.File{}
	if binderHandle != 0 {
		files = append(files, os.NewFile(uintptr(binderHandle), "binder"))
		envs = append(envs, "SKEWER_HAS_BINDER=TRUE")
	}
	if loggerHandle != 0 {
		files = append(files, os.NewFile(uintptr(loggerHandle), "logger"))
		envs = append(envs, "SKEWER_HAS_LOGGER=TRUE")
	}
	if messagePipe != nil {
		files = append(files, messagePipe)
		envs = append(envs, "SKEWER_HAS_PIPE=TRUE")
	}
	if test {
		envs = append(envs, "SKEWER_TEST=TRUE")
	}

	cmd := &exec.Cmd{
		Path:       exe,
		Stderr:     os.Stderr,
		ExtraFiles: files,
		Env:        envs,
		Args:       []string{name},
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	return cmd, in, out, nil
}

func (s *PluginController) Create(test bool, dumpable bool, storePath string, confDir string) error {
	// if the provider process already lives, Create() just returns
	s.createdMu.Lock()
	if s.created {
		s.createdMu.Unlock()
		return nil
	}

	s.ShutdownChan = make(chan struct{})
	s.ExitCode = 0

	var err error
	name := ReverseNetworkServiceMap[s.typ]

	switch s.typ {
	case RELP, TCP, UDP:
		// if creating the namespaces fails, fallback to classical start
		// this way we can support environments where user namespaces are not
		// available
		s.cmd, s.stdin, s.stdout, err = setupCmd(fmt.Sprintf("confined-%s", name), s.binderHandle, s.loggerHandle, nil, test)
		if err != nil {
			close(s.ShutdownChan)
			s.createdMu.Unlock()
			return err
		}

		err = sys.StartInNamespaces(s.cmd, dumpable, "", "")

		if err != nil {
			s.logger.Warn("Starting plugin in user namespace failed", "error", err, "type", name)
			s.cmd, s.stdin, s.stdout, err = setupCmd(name, s.binderHandle, s.loggerHandle, nil, test)
			if err != nil {
				close(s.ShutdownChan)
				s.createdMu.Unlock()
				return err
			}
			err = s.cmd.Start()
		}

	case Store:
		piper, pipew, err := os.Pipe()
		if err != nil {
			close(s.ShutdownChan)
			s.createdMu.Unlock()
			return err
		}
		s.pipe = pipew
		s.cmd, s.stdin, s.stdout, err = setupCmd(fmt.Sprintf("confined-%s", name), s.binderHandle, s.loggerHandle, piper, test)
		if err != nil {
			piper.Close()
			pipew.Close()
			close(s.ShutdownChan)
			s.createdMu.Unlock()
			return err
		}

		err = sys.StartInNamespaces(s.cmd, dumpable, storePath, "")

		if err != nil {
			s.logger.Warn("Starting plugin in user namespace failed", "error", err, "type", name)
			s.cmd, s.stdin, s.stdout, err = setupCmd(name, s.binderHandle, s.loggerHandle, piper, test)
			if err != nil {
				piper.Close()
				pipew.Close()
				close(s.ShutdownChan)
				s.createdMu.Unlock()
				return err
			}
			err = s.cmd.Start()
		}
		piper.Close()
		if err != nil {
			pipew.Close()
		}

	case Journal:
		s.cmd, s.stdin, s.stdout, err = setupCmd(fmt.Sprintf("confined-%s", name), s.binderHandle, s.loggerHandle, nil, test)
		//s.cmd, s.stdin, s.stdout, err = setupCmd(name, s.binderHandle, s.loggerHandle, test)

		if err != nil {
			close(s.ShutdownChan)
			s.createdMu.Unlock()
			return err
		}

		err = sys.StartInNamespaces(s.cmd, dumpable, "", "")

		if err != nil {
			s.logger.Warn("Starting plugin in user namespace failed", "error", err, "type", name)
			s.cmd, s.stdin, s.stdout, err = setupCmd(name, s.binderHandle, s.loggerHandle, nil, test)
			if err != nil {
				close(s.ShutdownChan)
				s.createdMu.Unlock()
				return err
			}
			err = s.cmd.Start()
		}

	default:
		s.cmd, s.stdin, s.stdout, err = setupCmd(name, s.binderHandle, s.loggerHandle, nil, test)
		if err != nil {
			close(s.ShutdownChan)
			s.createdMu.Unlock()
			return err
		}
		err = s.cmd.Start()
	}

	if err != nil {
		close(s.ShutdownChan)
		s.createdMu.Unlock()
		return err
	}
	s.created = true
	s.createdMu.Unlock()

	go func() {
		// monitor plugin process termination
		err := s.cmd.Wait()
		if err == nil {
			s.logger.Debug("Plugin process has exited without reporting error", "type", name)
			s.ExitCode = 0
		} else if e, ok := err.(*exec.ExitError); ok {
			s.logger.Error("Plugin process has shutdown with error", "stderr", string(e.Stderr), "type", name, "error", e.Error())
			status := e.ProcessState.Sys()
			if cstatus, ok := status.(syscall.WaitStatus); ok {
				s.ExitCode = cstatus.ExitStatus()
				s.logger.Error("Plugin process return code", "type", name, "code", s.ExitCode)
			} else {
				s.ExitCode = -1
				s.logger.Warn("Could not interpret plugin process return code", "type", name)
			}
		} else {
			s.logger.Error("Plugin process has exited abnormally, but the error could not be interpreted", "type", name, "error", err.Error())
		}
		close(s.ShutdownChan)
		// after some client has waited ShutdownChan to be closed, it can safely read ExitCode
	}()
	return nil

}

type StorePlugin struct {
	*PluginController
	*utils.MessageQueue
	stopChan chan struct{}
	pushwg   *sync.WaitGroup
}

func (s *StorePlugin) pushqueue() {
	var messages []*model.TcpUdpParsedMessage
	var message *model.TcpUdpParsedMessage
	var messageb []byte
	var err error
	for {
		messages = s.MessageQueue.GetMany(1000)
		if len(messages) == 0 {
			return
		}
		for _, message = range messages {
			messageb, err = message.MarshalMsg(nil)
			if err == nil {
				_, err = s.pipe.Write(messageb)
				if err != nil {
					fmt.Fprintln(os.Stderr, "BLLLALALA", err)
				}
			} else {
				fmt.Fprintln(os.Stderr, "OHHHHENOCDE", err, message)
			}
		}
	}
}

func (s *StorePlugin) push() {
	for s.MessageQueue.Wait(s.stopChan) {
		s.pushqueue()
	}
	s.pushwg.Done()
}

func (s *StorePlugin) Shutdown(killTimeOut time.Duration) {
	close(s.stopChan)                        // will make push() return
	s.pushwg.Wait()                          // wait that push() returns
	s.pushqueue()                            // empty the queue, in case there are pending messages
	s.pipe.Close()                           // signal the child that we are done sending messages
	s.PluginController.Shutdown(killTimeOut) // shutdown the child
}

// Stash stores the given message into the Store
func (s *StorePlugin) Stash(m *model.TcpUdpParsedMessage) (fatal error, nonfatal error) {
	// this method is called very frequently, so we avoid to lock anything
	// the MessageQueue ensures that we write the messages sequentially to the store child
	s.MessageQueue.Put(m)
	return nil, nil
}

func NewStorePlugin(loggerHandle int, l log15.Logger) *StorePlugin {
	s := &StorePlugin{PluginController: NewPluginController(Store, nil, nil, 0, loggerHandle, l)}
	s.MessageQueue = utils.NewMessageQueue()
	s.stopChan = make(chan struct{})
	s.pushwg = &sync.WaitGroup{}
	s.pushwg.Add(1)
	go s.push()
	return s
}