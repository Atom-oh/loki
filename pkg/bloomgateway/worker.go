package bloomgateway

import (
	"context"
	"fmt"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/services"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/common/model"
	"golang.org/x/exp/slices"

	"github.com/grafana/loki/pkg/queue"
	v1 "github.com/grafana/loki/pkg/storage/bloom/v1"
	"github.com/grafana/loki/pkg/storage/stores/shipper/bloomshipper"
)

type workerConfig struct {
	maxWaitTime time.Duration
	maxItems    int
}

type workerMetrics struct {
	dequeuedTasks      *prometheus.CounterVec
	dequeueErrors      *prometheus.CounterVec
	dequeueWaitTime    *prometheus.SummaryVec
	storeAccessLatency *prometheus.HistogramVec
	bloomQueryLatency  *prometheus.HistogramVec
}

func newWorkerMetrics(registerer prometheus.Registerer, namespace, subsystem string) *workerMetrics {
	labels := []string{"worker"}
	return &workerMetrics{
		dequeuedTasks: promauto.With(registerer).NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "dequeued_tasks_total",
			Help:      "Total amount of tasks that the worker dequeued from the bloom query queue",
		}, labels),
		dequeueErrors: promauto.With(registerer).NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "dequeue_errors_total",
			Help:      "Total amount of failed dequeue operations",
		}, labels),
		dequeueWaitTime: promauto.With(registerer).NewSummaryVec(prometheus.SummaryOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "dequeue_wait_time",
			Help:      "Time spent waiting for dequeuing tasks from queue",
		}, labels),
		bloomQueryLatency: promauto.With(registerer).NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "bloom_query_latency",
			Help:      "Latency in seconds of processing bloom blocks",
		}, append(labels, "status")),
		// TODO(chaudum): Move this metric into the bloomshipper
		storeAccessLatency: promauto.With(registerer).NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "store_latency",
			Help:      "Latency in seconds of accessing the bloom store component",
		}, append(labels, "operation")),
	}
}

// worker is a datastructure that consumes tasks from the request queue,
// processes them and returns the result/error back to the response channels of
// the tasks.
// It is responsible for multiplexing tasks so they can be processes in a more
// efficient way.
type worker struct {
	services.Service

	id      string
	cfg     workerConfig
	queue   *queue.RequestQueue
	shipper bloomshipper.Interface
	tasks   *pendingTasks
	logger  log.Logger
	metrics *workerMetrics
}

func newWorker(id string, cfg workerConfig, queue *queue.RequestQueue, shipper bloomshipper.Interface, tasks *pendingTasks, logger log.Logger, metrics *workerMetrics) *worker {
	w := &worker{
		id:      id,
		cfg:     cfg,
		queue:   queue,
		shipper: shipper,
		tasks:   tasks,
		logger:  log.With(logger, "worker", id),
		metrics: metrics,
	}
	w.Service = services.NewBasicService(w.starting, w.running, w.stopping).WithName(id)
	return w
}

func (w *worker) starting(_ context.Context) error {
	level.Debug(w.logger).Log("msg", "starting worker")
	w.queue.RegisterConsumerConnection(w.id)
	return nil
}

func (w *worker) running(ctx context.Context) error {
	idx := queue.StartIndexWithLocalQueue

	for {
		select {

		case <-ctx.Done():
			return errors.Wrapf(ctx.Err(), "shutting down worker %s", w.id)

		default:
			iterationCtx := context.Background()
			dequeueStart := time.Now()
			items, newIdx, err := w.queue.DequeueMany(iterationCtx, idx, w.id, w.cfg.maxItems, w.cfg.maxWaitTime)
			w.metrics.dequeueWaitTime.WithLabelValues(w.id).Observe(time.Since(dequeueStart).Seconds())
			if err != nil {
				// We only return an error if the queue is stopped and dequeuing did not yield any items
				if err == queue.ErrStopped && len(items) == 0 {
					level.Error(w.logger).Log("msg", "queue is stopped")
					return err
				}
				w.metrics.dequeueErrors.WithLabelValues(w.id).Inc()
				level.Error(w.logger).Log("msg", "failed to dequeue tasks", "err", err, "items", len(items))
			}
			idx = newIdx

			if len(items) == 0 {
				w.queue.ReleaseRequests(items)
				continue
			}
			w.metrics.dequeuedTasks.WithLabelValues(w.id).Add(float64(len(items)))

			tasksByDay := make(map[time.Time][]Task)

			for _, item := range items {
				if item == nil {
					// this should never happen, but it's a safety measure
					w.queue.ReleaseRequests(items)
					return errors.New("dequeued item is nil")
				}
				task, ok := item.(Task)
				if !ok {
					// This really should never happen, because only the bloom gateway itself can enqueue tasks.
					w.queue.ReleaseRequests(items)
					return errors.Errorf("failed to cast dequeued item to Task: %v", item)
				}
				level.Debug(w.logger).Log("msg", "dequeued task", "task", task.ID, "closed", task.closed)
				w.tasks.Delete(task.ID)

				// check if task was already cancelled while it was waiting in the queue
				if task.Err() != nil {
					level.Debug(w.logger).Log("msg", "skipping cancelled task", "task", task.ID, "err", task.Err())
					task.Close()
					continue
				}

				fromDay, throughDay := task.Bounds()

				if fromDay.Equal(throughDay) {
					tasksByDay[fromDay] = append(tasksByDay[fromDay], task)
				} else {
					level.Debug(w.logger).Log("msg", "task spans across multiple days", "from", fromDay, "through", throughDay)
					for i := fromDay; i.Before(throughDay); i = i.Add(Day) {
						tasksByDay[i] = append(tasksByDay[i], task)
					}
				}
			}

			for day, tasks := range tasksByDay {
				logger := log.With(w.logger, "day", day)
				level.Debug(logger).Log("msg", "tasks per day", "tasks_len", len(tasks))
				for _, task := range tasks {
					level.Debug(w.logger).Log("msg", "individual task", "task", task.ID, "closed", task.closed)
				}

				// Remove tasks that are already cancelled
				tasks = slices.DeleteFunc(tasks, func(t Task) bool {
					return t.Err() != nil
				})
				level.Debug(logger).Log("msg", "not cancelled tasks per day", "tasks_len", len(tasks))
				// no tasks to process, continue with next day
				if len(tasks) == 0 {
					level.Debug(logger).Log("msg", "no tasks to process, continue with next day")
					continue
				}

				level.Debug(logger).Log("msg", "process tasks", "tasks", len(tasks))

				storeFetchStart := time.Now()
				blockRefs, err := w.shipper.GetBlockRefs(iterationCtx, tasks[0].Tenant, toModelTime(day), toModelTime(day.Add(Day).Add(-1*time.Nanosecond)))
				w.metrics.storeAccessLatency.WithLabelValues(w.id, "GetBlockRefs").Observe(time.Since(storeFetchStart).Seconds())
				if err != nil {
					level.Debug(logger).Log("msg", "error processing tasks. notifying all task's channels and go to the next day", "err", err)
					// send error to error channel of each task
					for _, t := range tasks {
						t.ErrCh <- err
					}
					// continue with tasks of next day
					continue
				}
				// No blocks found.
				// Since there are no blocks for the given tasks, we need to return the
				// unfiltered list of chunk refs.
				if len(blockRefs) == 0 {
					level.Warn(logger).Log("msg", "no blocks found")
					for _, t := range tasks {
						for _, ref := range t.Request.Refs {
							t.ResCh <- v1.Output{
								Fp:       model.Fingerprint(ref.Fingerprint),
								Removals: nil,
							}
						}
					}
					// continue with tasks of next day
					continue
				}

				partitionedTasks := partitionFingerprintRange(tasks, blockRefs)
				level.Debug(logger).Log("msg", "partitioned tasks", "regular", len(tasks), "partitioned", len(partitionedTasks))

				err = w.processBlocksWithCallback(iterationCtx, tasks[0].Tenant, day, partitionedTasks)
				if err != nil {
					level.Error(logger).Log("msg", "processed with an error", "err", err)
					// send error to error channel of each task
					for _, t := range tasks {
						t.ErrCh <- err
					}
					// continue with tasks of next day
					continue
				}
			}

			// close channels because everything is sent
			for _, tasks := range tasksByDay {
				for _, task := range tasks {
					level.Debug(w.logger).Log("msg", "close task", "task", task.ID, "closed", task.closed)
					task.Close()
				}
			}

			// return dequeued items back to the pool
			w.queue.ReleaseRequests(items)
		}
	}
}

func (w *worker) stopping(err error) error {
	level.Debug(w.logger).Log("msg", "stopping worker", "err", err)
	w.queue.UnregisterConsumerConnection(w.id)
	return nil
}

func (w *worker) processBlocksWithCallback(ctx context.Context, tenant string, day time.Time, partitionedTasks []boundedTasks) error {
	logger := log.With(w.logger, "worker", w.id)
	level.Debug(logger).Log("msg", "processBlocksWithCallback")
	defer func() {
		level.Debug(logger).Log("msg", "leaving processBlocksWithCallback")
	}()
	blockRefs := make([]bloomshipper.BlockRef, 0, len(partitionedTasks))
	for _, pt := range partitionedTasks {
		blockRefs = append(blockRefs, pt.blockRef)
	}
	return w.shipper.Fetch(ctx, tenant, blockRefs, func(bq *v1.BlockQuerier, minFp, maxFp uint64) error {
		logger := log.With(w.logger, "worker", w.id)
		level.Debug(logger).Log("msg", "inside callback")
		defer func() {
			level.Debug(logger).Log("msg", "leaving callback")
		}()
		for _, pt := range partitionedTasks {
			if pt.blockRef.MinFingerprint == minFp && pt.blockRef.MaxFingerprint == maxFp {
				return w.processBlock(ctx, bq, day, pt.tasks)
			}
		}
		return fmt.Errorf("no overlapping blocks for range %x-%x", minFp, maxFp)
	})
}

func (w *worker) processBlock(ctx context.Context, blockQuerier *v1.BlockQuerier, day time.Time, tasks []Task) error {
	logger := log.With(w.logger, "worker", w.id)
	level.Debug(logger).Log("msg", "start processBlock")
	defer func() {
		level.Debug(logger).Log("msg", "end processBlock")
	}()

	schema, err := blockQuerier.Schema()
	if err != nil {
		return err
	}

	level.Debug(logger).Log("msg", "creating tokenizer")
	tokenizer := v1.NewNGramTokenizer(schema.NGramLen(), 0)
	level.Debug(logger).Log("msg", "creating taskMergeIterator")
	it := newTaskMergeIterator(day, tokenizer, tasks...)
	level.Debug(logger).Log("msg", "Fuse")
	fq := blockQuerier.Fuse([]v1.PeekingIterator[v1.Request]{it})

	if ctx.Err() != nil {
		level.Debug(logger).Log("msg", "context error", "err", err)
		return ctx.Err()
	}

	for _, t := range tasks {
		if t.Err() != nil {
			level.Debug(logger).Log("msg", "task context error", "task", t.ID, "err", t.Err())
			return t.ctx.Err()
		}
	}

	start := time.Now()
	level.Debug(logger).Log("msg", "before fq.Run()")
	err = fq.Run()
	level.Debug(logger).Log("msg", "after fq.Run()")
	duration := time.Since(start).Seconds()

	if err != nil {
		level.Debug(logger).Log("msg", "completed with error", "err", err)
		w.metrics.bloomQueryLatency.WithLabelValues(w.id, "failure").Observe(duration)
		return err
	}

	w.metrics.bloomQueryLatency.WithLabelValues(w.id, "success").Observe(duration)
	return nil
}

func toModelTime(t time.Time) model.Time {
	return model.TimeFromUnixNano(t.UnixNano())
}
