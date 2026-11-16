package nodeenrollment

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"io.yueyang/tom/internal/nodeapi"
	"io.yueyang/tom/internal/nodecatalog"
	"io.yueyang/tom/internal/store/postgres"
)

type Directory interface {
	List(context.Context, string, int) ([]nodeapi.Endpoint, error)
}

// Fleet 为每个接入节点维护独立的心跳和持久验收流程；目录有界，节点间失败互不阻塞。
type Fleet struct {
	directory    Directory
	control      *postgres.Database
	runtime      nodecatalog.VerificationRuntime
	observer     nodecatalog.ObserverOptions
	verification nodecatalog.VerificationOptions
	maximum      int
	logger       *slog.Logger
}

func NewFleet(directory Directory, control *postgres.Database, runtime nodecatalog.VerificationRuntime, observer nodecatalog.ObserverOptions, verification nodecatalog.VerificationOptions, maximum int, logger *slog.Logger) (*Fleet, error) {
	if directory == nil || control == nil || control.Role() != postgres.ControlRole || runtime == nil || observer.Validate() != nil || verification.ValidateBudgets() != nil || maximum < 1 || maximum > 1000 || logger == nil {
		return nil, errors.New("多节点监督配置无效")
	}
	return &Fleet{directory, control, runtime, observer, verification, maximum, logger}, nil
}

// Run 定期发现新增节点；目录短暂失败保留现有监督器，取消时等待所有节点释放租约与连接。
func (fleet *Fleet) Run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	var workers sync.WaitGroup
	defer workers.Wait()
	defer cancel()
	type worker struct {
		imageID string
		stop    context.CancelFunc
		done    chan struct{}
	}
	known := map[string]worker{}
	failures := make(chan error, fleet.maximum*2)
	scan := func() error {
		after := ""
		for {
			bounded, stop := context.WithTimeout(ctx, fleet.observer.CallTimeout)
			endpoints, err := fleet.directory.List(bounded, after, 100)
			stop()
			if err != nil {
				return err
			}
			for _, endpoint := range endpoints {
				if endpoint.NodeID <= after {
					return errors.New("节点目录排序无效")
				}
				after = endpoint.NodeID
				if old, exists := known[endpoint.NodeID]; exists {
					if old.imageID == endpoint.ImageID {
						continue
					}
					old.stop()
					select {
					case <-old.done:
						delete(known, endpoint.NodeID)
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				if len(known) >= fleet.maximum {
					return ErrCapacity
				}
				catalog, err := nodecatalog.New(fleet.control, endpoint.NodeID)
				if err != nil {
					return err
				}
				observer, err := nodecatalog.NewObserver(catalog, fleet.runtime, fleet.observer, fleet.logger)
				if err != nil {
					return err
				}
				options := fleet.verification
				options.ImageID = endpoint.ImageID
				verifier, err := nodecatalog.NewVerifier(catalog, fleet.runtime, options, fleet.logger)
				if err != nil {
					return err
				}
				workerContext, stopWorker := context.WithCancel(ctx)
				done := make(chan struct{})
				known[endpoint.NodeID] = worker{endpoint.ImageID, stopWorker, done}
				var pending sync.WaitGroup
				for _, run := range []func(context.Context) error{observer.Run, verifier.Run} {
					workers.Add(1)
					pending.Add(1)
					go func() {
						defer workers.Done()
						defer pending.Done()
						err := run(workerContext)
						if workerContext.Err() == nil {
							failures <- errors.Join(errors.New("节点监督组件意外退出"), err)
						}
					}()
				}
				go func() { pending.Wait(); close(done) }()
				fleet.logger.InfoContext(ctx, "开始监督已接入节点", "node_id", endpoint.NodeID, "image_id", endpoint.ImageID)
			}
			if len(endpoints) < 100 {
				return nil
			}
		}
	}
	state := ""
	for {
		err := scan()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		next := "confirmed"
		if err != nil {
			next = "unconfirmed"
		}
		if next != state {
			if err != nil {
				fleet.logger.WarnContext(ctx, "节点接入目录读取未确认，保留已有监督并稍后重试", "known_nodes", len(known))
			} else {
				fleet.logger.InfoContext(ctx, "节点接入目录读取已确认", "known_nodes", len(known))
			}
			state = next
		}
		timer := time.NewTimer(fleet.observer.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case err := <-failures:
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
}
