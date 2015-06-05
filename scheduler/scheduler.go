package scheduler

import (
	"errors"
	"fmt"
	"time"

	"github.com/intelsdi-x/pulse/core"
	"github.com/intelsdi-x/pulse/core/cdata"
	"github.com/intelsdi-x/pulse/core/ctypes"
	"github.com/intelsdi-x/pulse/pkg/schedule"
	"github.com/intelsdi-x/pulse/scheduler/wmap"
)

var (
	MetricManagerNotSet = errors.New("MetricManager is not set.")
	SchedulerNotStarted = errors.New("Scheduler is not started.")
)

type schedulerState int

const (
	schedulerStopped schedulerState = iota
	schedulerStarted
)

// ManagesMetric is implemented by control
// On startup a scheduler will be created and passed a reference to control
type ManagesMetrics interface {
	SubscribeMetricType(mt core.RequestedMetric, cd *cdata.ConfigDataNode) (core.Metric, []error)
	UnsubscribeMetricType(mt core.Metric)
	CollectsMetrics
	PublishesMetrics
	ProcessesMetrics
	ManagesPluginContentTypes
}

// ManagesPluginContentTypes is an interface to a plugin manager that can tell us what content accept and returns are supported.
type ManagesPluginContentTypes interface {
	GetPluginContentTypes(n string, t core.PluginType, v int) ([]string, []string, error)
}

type CollectsMetrics interface {
	CollectMetrics([]core.Metric, time.Time) ([]core.Metric, []error)
}

type PublishesMetrics interface {
	PublishMetrics(contentType string, content []byte, pluginName string, pluginVersion int, config map[string]ctypes.ConfigValue) []error
}

type ProcessesMetrics interface {
	ProcessMetrics(contentType string, content []byte, pluginName string, pluginVersion int, config map[string]ctypes.ConfigValue) (string, []byte, []error)
}

type scheduler struct {
	workManager   *workManager
	metricManager ManagesMetrics
	tasks         *taskCollection
	state         schedulerState
}

type managesWork interface {
	Work(job) job
}

// New returns an instance of the scheduler
// The MetricManager must be set before the scheduler can be started.
// The MetricManager must be started before it can be used.
func New(opts ...workManagerOption) *scheduler {
	s := &scheduler{
		tasks: newTaskCollection(),
	}

	// we are setting the size of the queue and number of workers for
	// collect, process and publish consistently for now
	s.workManager = newWorkManager(opts...)
	s.workManager.Start()

	return s
}

type taskErrors struct {
	errs []error
}

func (t *taskErrors) Errors() []error {
	return t.errs
}

// CreateTask creates and returns task
func (s *scheduler) CreateTask(sch schedule.Schedule, wfMap *wmap.WorkflowMap, opts ...core.TaskOption) (core.Task, core.TaskErrors) {
	// Create a container for task errors
	te := &taskErrors{
		errs: make([]error, 0),
	}

	// Return error if we are not started.
	if s.state != schedulerStarted {
		te.errs = append(te.errs, SchedulerNotStarted)
		return nil, te
	}

	// Ensure the schedule is valid at this point and time.
	if err := sch.Validate(); err != nil {
		te.errs = append(te.errs, err)
		return nil, te
	}

	// Generate a workflow from the workflow map
	wf, err := wmapToWorkflow(wfMap)
	if err != nil {
		te.errs = append(te.errs, SchedulerNotStarted)
		return nil, te
	}

	// Bind plugin content type selections in workflow
	err = wf.BindPluginContentTypes(s.metricManager)

	// Subscribe to MT.
	// If we encounter an error we will unwind successful subscriptions.
	subscriptions := make([]core.Metric, 0)
	for _, m := range wf.metrics {
		cdt, er := wfMap.CollectNode.GetConfigTree()
		if er != nil {
			te.errs = append(te.errs, er)
			continue
		}
		cd := cdt.Get(m.Namespace())
		mt, err := s.metricManager.SubscribeMetricType(m, cd)
		if err == nil {
			subscriptions = append(subscriptions, mt)
		} else {
			te.errs = append(te.errs, err...)
		}
	}

	// Unwind successful subscriptions if we got here with errors (idempotent)
	if len(te.errs) > 0 {
		for _, sub := range subscriptions {
			s.metricManager.UnsubscribeMetricType(sub)
		}
		return nil, te
	}

	// Create the task object
	task := newTask(sch, subscriptions, wf, s.workManager, s.metricManager, opts...)

	// Add task to taskCollection
	if err := s.tasks.add(task); err != nil {
		te.errs = append(te.errs, err)
		return nil, te
	}

	return task, te
}

//GetTasks returns a copy of the tasks in a map where the task id is the key
func (s *scheduler) GetTasks() map[uint64]core.Task {
	tasks := make(map[uint64]core.Task)
	for id, t := range s.tasks.Table() {
		tasks[id] = t
	}
	return tasks
}

//GetTask provided the task id a task is returned
func (s *scheduler) GetTask(id uint64) (core.Task, error) {
	task := s.tasks.Get(id)
	if task == nil {
		return nil, errors.New(fmt.Sprintf("No task with Id '%v'", id))
	}
	return task, nil
}

// Start starts the scheduler
func (s *scheduler) Start() error {
	if s.metricManager == nil {
		return MetricManagerNotSet
	}
	s.state = schedulerStarted
	return nil
}

func (s *scheduler) Stop() {
	s.state = schedulerStopped
}

// Set metricManager for scheduler
func (s *scheduler) SetMetricManager(mm ManagesMetrics) {
	s.metricManager = mm
}