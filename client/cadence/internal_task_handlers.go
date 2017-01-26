package cadence

// All code in this file is private to the package.

import (
	"fmt"
	"reflect"
	"time"

	"github.com/uber-common/bark"
	"github.com/uber-go/tally"

	m "code.uber.internal/devexp/minions-client-go.git/.gen/go/minions"
	s "code.uber.internal/devexp/minions-client-go.git/.gen/go/shared"
	"code.uber.internal/devexp/minions-client-go.git/common"
	"code.uber.internal/devexp/minions-client-go.git/common/metrics"
	"golang.org/x/net/context"
)

// interfaces
type (
	// workflowTaskHandler represents workflow task handlers.
	workflowTaskHandler interface {
		// Process the workflow task
		ProcessWorkflowTask(task *workflowTask, emitStack bool) (response *s.RespondDecisionTaskCompletedRequest, stackTrace string, err error)
	}

	// activityTaskHandler represents activity task handlers.
	activityTaskHandler interface {
		// Execute the activity task
		// The return interface{} can have three requests, use switch to find the type of it.
		// - RespondActivityTaskCompletedRequest
		// - RespondActivityTaskFailedRequest
		// - RespondActivityTaskCancelRequest
		Execute(context context.Context, task *activityTask) (interface{}, error)
	}

	// workflowExecutionEventHandler process a single event.
	workflowExecutionEventHandler interface {
		// Process a single event and return the assosciated decisions.
		ProcessEvent(event *s.HistoryEvent) ([]*s.Decision, error)
		StackTrace() string
		// Close for cleaning up resources on this event handler
		Close()
	}

	// workflowTask wraps a decision task.
	workflowTask struct {
		task *s.PollForDecisionTaskResponse
	}

	// activityTask wraps a activity task.
	activityTask struct {
		task *s.PollForActivityTaskResponse
	}
)

type (
	// workflowTaskHandlerImpl is the implementation of workflowTaskHandler
	workflowTaskHandlerImpl struct {
		taskListName       string
		identity           string
		workflowDefFactory workflowDefinitionFactory
		metricsScope       tally.Scope
		ppMgr              pressurePointMgr
		logger             bark.Logger
	}

	// activityTaskHandlerImpl is the implementation of ActivityTaskHandler
	activityTaskHandlerImpl struct {
		taskListName    string
		identity        string
		implementations map[ActivityType]Activity
		service         m.TChanWorkflowService
		metricsScope    tally.Scope
		logger          bark.Logger
	}

	// eventsHelper wrapper method to help information about events.
	eventsHelper struct {
		workflowTask *workflowTask
	}

	// activityTaskFailedError wraps the details of the failure of activity
	activityTaskFailedError struct {
		reason  string
		details []byte
	}

	// activityTaskTimeoutError wraps the details of the timeout of activity
	activityTaskTimeoutError struct {
		TimeoutType s.TimeoutType
	}
)

// Error from error.Error
func (e activityTaskFailedError) Error() string {
	return fmt.Sprintf("Reason: %s, Details: %s", e.reason, e.details)
}

// Details of the error
func (e activityTaskFailedError) Details() []byte {
	return e.details
}

// Reason of the error
func (e activityTaskFailedError) Reason() string {
	return e.reason
}

// Error from error.Error
func (e activityTaskTimeoutError) Error() string {
	return fmt.Sprintf("TimeoutType: %v", e.TimeoutType)
}

// Details of the error
func (e activityTaskTimeoutError) Details() []byte {
	return nil
}

// Reason of the error
func (e activityTaskTimeoutError) Reason() string {
	return e.Error()
}

// Get last non replayed event ID.
func (eh eventsHelper) LastNonReplayedID() int64 {
	if eh.workflowTask.task.PreviousStartedEventId == nil {
		return 0
	}
	return *eh.workflowTask.task.PreviousStartedEventId
}

// newWorkflowTaskHandler returns an implementation of workflow task handler.
func newWorkflowTaskHandler(taskListName string, identity string, factory workflowDefinitionFactory,
	logger bark.Logger, metricsScope tally.Scope, ppMgr pressurePointMgr) workflowTaskHandler {
	return &workflowTaskHandlerImpl{
		taskListName:       taskListName,
		identity:           identity,
		workflowDefFactory: factory,
		logger:             logger,
		ppMgr:              ppMgr,
		metricsScope:       metricsScope}
}

// ProcessWorkflowTask processes each all the events of the workflow task.
func (wth *workflowTaskHandlerImpl) ProcessWorkflowTask(workflowTask *workflowTask, emitStack bool) (result *s.RespondDecisionTaskCompletedRequest, stackTrace string, err error) {
	if workflowTask == nil {
		return nil, "", fmt.Errorf("nil workflowtask provided")
	}

	wth.logger.Debugf("Processing New Workflow Task: Type=%s, PreviousStartedEventId=%d",
		workflowTask.task.GetWorkflowType().GetName(), workflowTask.task.GetPreviousStartedEventId())

	// Setup workflow Info
	workflowInfo := &WorkflowInfo{
		WorkflowType: flowWorkflowTypeFrom(*workflowTask.task.WorkflowType),
		TaskListName: wth.taskListName,
		// workflowExecution
	}

	isWorkflowCompleted := false
	var completionResult []byte
	var failure Error

	completeHandler := func(result []byte, err Error) {
		completionResult = result
		failure = err
		isWorkflowCompleted = true
	}

	eventHandler := newWorkflowExecutionEventHandler(
		workflowInfo, wth.workflowDefFactory, completeHandler, wth.logger)
	helperEvents := &eventsHelper{workflowTask: workflowTask}
	history := workflowTask.task.History
	decisions := []*s.Decision{}

	startTime := time.Now()

	// Process events
	for _, event := range history.Events {
		wth.logger.Debugf("ProcessWorkflowTask: Id=%d, Event=%+v", event.GetEventId(), event)

		isInReplay := event.GetEventId() < helperEvents.LastNonReplayedID()

		// Any metrics.
		if wth.metricsScope != nil && !isInReplay {
			switch event.GetEventType() {
			case s.EventType_DecisionTaskTimedOut:
				wth.metricsScope.Counter(metrics.DecisionsTimeoutCounter).Inc(1)
			}
		}

		// Any pressure points.
		err := wth.executeAnyPressurePoints(event, isInReplay)
		if err != nil {
			return nil, "", err
		}

		eventDecisions, err := eventHandler.ProcessEvent(event)
		if err != nil {
			return nil, "", err
		}

		if !isInReplay {
			if eventDecisions != nil {
				decisions = append(decisions, eventDecisions...)
			}
		}
	}

	eventDecisions := wth.completeWorkflow(isWorkflowCompleted, completionResult, failure)
	if len(eventDecisions) > 0 {
		decisions = append(decisions, eventDecisions...)

		if wth.metricsScope != nil {
			wth.metricsScope.Counter(metrics.WorkflowsCompletionTotalCounter).Inc(1)
			elapsed := time.Now().Sub(startTime)
			wth.metricsScope.Timer(metrics.WorkflowEndToEndLatency).Record(elapsed)
		}
	}

	// Fill the response.
	taskCompletionRequest := &s.RespondDecisionTaskCompletedRequest{
		TaskToken: workflowTask.task.TaskToken,
		Decisions: decisions,
		Identity:  common.StringPtr(wth.identity),
		// ExecutionContext:
	}
	if emitStack {
		stackTrace = eventHandler.StackTrace()
	}
	return taskCompletionRequest, stackTrace, nil
}

func (wth *workflowTaskHandlerImpl) completeWorkflow(isWorkflowCompleted bool, completionResult []byte,
	err Error) []*s.Decision {
	decisions := []*s.Decision{}
	if err != nil {
		// Workflow failures
		failDecision := createNewDecision(s.DecisionType_FailWorkflowExecution)
		failDecision.FailWorkflowExecutionDecisionAttributes = &s.FailWorkflowExecutionDecisionAttributes{
			Reason:  common.StringPtr(err.Reason()),
			Details: err.Details(),
		}
		decisions = append(decisions, failDecision)
	} else if isWorkflowCompleted {
		// Workflow completion
		completeDecision := createNewDecision(s.DecisionType_CompleteWorkflowExecution)
		completeDecision.CompleteWorkflowExecutionDecisionAttributes = &s.CompleteWorkflowExecutionDecisionAttributes{
			Result_: completionResult,
		}
		decisions = append(decisions, completeDecision)
	}
	return decisions
}

func (wth *workflowTaskHandlerImpl) executeAnyPressurePoints(event *s.HistoryEvent, isInReplay bool) error {
	if wth.ppMgr != nil && !reflect.ValueOf(wth.ppMgr).IsNil() && !isInReplay {
		switch event.GetEventType() {
		case s.EventType_DecisionTaskStarted:
			return wth.ppMgr.Execute(PressurePointTypeDecisionTaskStartTimeout)
		case s.EventType_ActivityTaskScheduled:
			return wth.ppMgr.Execute(PressurePointTypeActivityTaskScheduleTimeout)
		case s.EventType_ActivityTaskStarted:
			return wth.ppMgr.Execute(PressurePointTypeActivityTaskStartTimeout)
		}
	}
	return nil
}

func newActivityTaskHandler(taskListName string, identity string, activities []Activity,
	service m.TChanWorkflowService, logger bark.Logger, metricsScope tally.Scope) activityTaskHandler {
	implementations := make(map[ActivityType]Activity)
	for _, a := range activities {
		implementations[a.ActivityType()] = a
	}
	return &activityTaskHandlerImpl{
		taskListName:    taskListName,
		identity:        identity,
		implementations: implementations,
		service:         service,
		logger:          logger,
		metricsScope:    metricsScope}
}

// Execute executes an implementation of the activity.
func (ath *activityTaskHandlerImpl) Execute(ctx context.Context, activityTask *activityTask) (interface{}, error) {
	t := activityTask.task
	ath.logger.Debugf("[WorkflowID: %s] Execute Activity: %s",
		t.GetWorkflowExecution().GetWorkflowId(), t.GetActivityType().GetName())

	ctx = context.WithValue(ctx, activityEnvContextKey, &activityEnvironment{
		taskToken:    t.TaskToken,
		identity:     ath.identity,
		service:      ath.service,
		activityType: ActivityType{Name: *t.ActivityType.Name},
		activityID:   *t.ActivityId,
		workflowExecution: WorkflowExecution{
			RunID: *t.WorkflowExecution.RunId,
			ID:    *t.WorkflowExecution.WorkflowId},
	})
	activityType := *t.GetActivityType()
	activityImplementation, ok := ath.implementations[flowActivityTypeFrom(activityType)]
	if !ok {
		// Couldn't find the activity implementation.
		return nil, fmt.Errorf("No implementation for activityType=%v", activityType)
	}

	output, err := activityImplementation.Execute(ctx, t.GetInput())
	if err != nil {
		responseFailure := &s.RespondActivityTaskFailedRequest{
			TaskToken: t.TaskToken,
			Reason:    common.StringPtr(err.Reason()),
			Details:   err.Details(),
			Identity:  common.StringPtr(ath.identity)}
		return responseFailure, nil
	}

	responseComplete := &s.RespondActivityTaskCompletedRequest{
		TaskToken: t.TaskToken,
		Result_:   output,
		Identity:  common.StringPtr(ath.identity)}
	return responseComplete, nil
}

func createNewDecision(decisionType s.DecisionType) *s.Decision {
	return &s.Decision{
		DecisionType: common.DecisionTypePtr(decisionType),
	}
}
