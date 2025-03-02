package scandataexport

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/goharbor/harbor/src/jobservice/job"
	"github.com/goharbor/harbor/src/jobservice/logger"
	"github.com/goharbor/harbor/src/lib/errors"
	"github.com/goharbor/harbor/src/lib/log"
	"github.com/goharbor/harbor/src/lib/orm"
	q2 "github.com/goharbor/harbor/src/lib/q"
	"github.com/goharbor/harbor/src/pkg/scan/export"
	"github.com/goharbor/harbor/src/pkg/systemartifact"
	"github.com/goharbor/harbor/src/pkg/task"
)

func init() {
	task.SetExecutionSweeperCount(job.ScanDataExport, 50)
}

var Ctl = NewController()

type Controller interface {
	Start(ctx context.Context, criteria export.Request) (executionID int64, err error)
	GetExecution(ctx context.Context, executionID int64) (*export.Execution, error)
	ListExecutions(ctx context.Context, userName string) ([]*export.Execution, error)
	GetTask(ctx context.Context, executionID int64) (*task.Task, error)
	DeleteExecution(ctx context.Context, executionID int64) error
}

func NewController() Controller {
	return &controller{
		execMgr:        task.ExecMgr,
		taskMgr:        task.Mgr,
		makeCtx:        orm.Context,
		sysArtifactMgr: systemartifact.Mgr,
	}
}

type controller struct {
	execMgr        task.ExecutionManager
	taskMgr        task.Manager
	makeCtx        func() context.Context
	sysArtifactMgr systemartifact.Manager
}

func (c *controller) ListExecutions(ctx context.Context, userName string) ([]*export.Execution, error) {
	keywords := make(map[string]interface{})
	keywords["VendorType"] = job.ScanDataExport
	keywords[fmt.Sprintf("ExtraAttrs.%s", export.UserNameAttribute)] = userName

	q := q2.New(q2.KeyWords{})
	q.Keywords = keywords
	execsForUser, err := c.execMgr.List(ctx, q)
	if err != nil {
		return nil, err
	}
	execs := make([]*export.Execution, 0)
	for _, execForUser := range execsForUser {
		execs = append(execs, c.convertToExportExecStatus(ctx, execForUser))
	}
	return execs, nil
}

func (c *controller) GetTask(ctx context.Context, executionID int64) (*task.Task, error) {
	query := q2.New(q2.KeyWords{})

	keywords := make(map[string]interface{})
	keywords["VendorType"] = job.ScanDataExport
	keywords["ExecutionID"] = executionID
	query.Keywords = keywords
	query.Sorts = append(query.Sorts, &q2.Sort{
		Key:  "ID",
		DESC: true,
	})
	tasks, err := c.taskMgr.List(ctx, query)
	if err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		return nil, errors.Errorf("No task found for execution Id : %d", executionID)
	}
	// for the export JOB there would be a single instance of the task corresponding to the execution
	// we will hence return the latest instance of the task associated with this execution
	logger.Infof("Returning task instance with ID : %d", tasks[0].ID)
	return tasks[0], nil
}

func (c *controller) GetExecution(ctx context.Context, executionID int64) (*export.Execution, error) {
	exec, err := c.execMgr.Get(ctx, executionID)
	if err != nil {
		logger.Errorf("Error when fetching execution status for ExecutionId: %d error : %v", executionID, err)
		return nil, err
	}
	if exec == nil {
		logger.Infof("No execution found for ExecutionId: %d", executionID)
		return nil, nil
	}
	return c.convertToExportExecStatus(ctx, exec), nil
}

func (c *controller) DeleteExecution(ctx context.Context, executionID int64) error {
	err := c.execMgr.Delete(ctx, executionID)
	if err != nil {
		logger.Errorf("Error when deleting execution  for ExecutionId: %d, error : %v", executionID, err)
	}
	return err
}

func (c *controller) Start(ctx context.Context, request export.Request) (executionID int64, err error) {
	logger := log.GetLogger(ctx)
	vendorID := int64(ctx.Value(export.CsvJobVendorIDKey).(int))
	extraAttrs := make(map[string]interface{})
	extraAttrs[export.JobNameAttribute] = request.JobName
	extraAttrs[export.UserNameAttribute] = request.UserName
	id, err := c.execMgr.Create(ctx, job.ScanDataExport, vendorID, task.ExecutionTriggerManual, extraAttrs)
	logger.Infof("Created an execution record with id : %d for vendorID: %d", id, vendorID)
	if err != nil {
		logger.Errorf("Encountered error when creating job : %v", err)
		return 0, err
	}

	// create a job object and fill with metadata and parameters
	params := make(map[string]interface{})
	params["JobId"] = id
	params["Request"] = request
	params[export.JobModeKey] = export.JobModeExport

	j := &task.Job{
		Name: job.ScanDataExport,
		Metadata: &job.Metadata{
			JobKind: job.KindGeneric,
		},
		Parameters: params,
	}

	_, err = c.taskMgr.Create(ctx, id, j)

	if err != nil {
		logger.Errorf("Unable to create a scan data export job: %v", err)
		c.markError(ctx, id, err)
		return 0, err
	}

	logger.Info("Created job for scan data export successfully")
	return id, nil
}

func (c *controller) markError(ctx context.Context, executionID int64, err error) {
	logger := log.GetLogger(ctx)
	// try to stop the execution first in case that some tasks are already created
	if err := c.execMgr.StopAndWait(ctx, executionID, 10*time.Second); err != nil {
		logger.Errorf("failed to stop the execution %d: %v", executionID, err)
	}
	if err := c.execMgr.MarkError(ctx, executionID, err.Error()); err != nil {
		logger.Errorf("failed to mark error for the execution %d: %v", executionID, err)
	}
}

func (c *controller) convertToExportExecStatus(ctx context.Context, exec *task.Execution) *export.Execution {
	execStatus := &export.Execution{
		ID:            exec.ID,
		UserID:        exec.VendorID,
		Status:        exec.Status,
		StatusMessage: exec.StatusMessage,
		Trigger:       exec.Trigger,
		StartTime:     exec.StartTime,
		EndTime:       exec.EndTime,
	}
	if digest, ok := exec.ExtraAttrs[export.DigestKey]; ok {
		execStatus.ExportDataDigest = digest.(string)
	}
	if jobName, ok := exec.ExtraAttrs[export.JobNameAttribute]; ok {
		execStatus.JobName = jobName.(string)
	}
	if userName, ok := exec.ExtraAttrs[export.UserNameAttribute]; ok {
		execStatus.UserName = userName.(string)
	}
	artifactExists := c.isCsvArtifactPresent(ctx, exec.ID, execStatus.ExportDataDigest)
	execStatus.FilePresent = artifactExists
	return execStatus
}

func (c *controller) isCsvArtifactPresent(ctx context.Context, execID int64, digest string) bool {
	repositoryName := fmt.Sprintf("scandata_export_%v", execID)
	exists, err := c.sysArtifactMgr.Exists(ctx, strings.ToLower(export.Vendor), repositoryName, digest)
	if err != nil {
		exists = false
	}
	return exists
}
