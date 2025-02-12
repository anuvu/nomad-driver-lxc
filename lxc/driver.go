package lxc

import (
	"context"
	"fmt"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad-driver-lxc/version"
	"github.com/hashicorp/nomad/client/stats"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	nstructs "github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	pstructs "github.com/hashicorp/nomad/plugins/shared/structs"
	lxc "github.com/lxc/go-lxc"
)

const (
	// pluginName is the name of the plugin
	pluginName = "lxc"

	// fingerprintPeriod is the interval at which the driver will send fingerprint responses
	fingerprintPeriod = 30 * time.Second

	// taskHandleVersion is the version of task handle which this driver sets
	// and understands how to decode driver state
	taskHandleVersion = 1
)

var (
	// pluginInfo is the response returned for the PluginInfo RPC
	pluginInfo = &base.PluginInfoResponse{
		Type:              base.PluginTypeDriver,
		PluginApiVersions: []string{drivers.ApiVersion010},
		PluginVersion:     version.Version,
		Name:              pluginName,
	}

	// configSpec is the hcl specification returned by the ConfigSchema RPC
	configSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		"enabled": hclspec.NewDefault(
			hclspec.NewAttr("enabled", "bool", false),
			hclspec.NewLiteral("true"),
		),
		"volumes_enabled": hclspec.NewDefault(
			hclspec.NewAttr("volumes_enabled", "bool", false),
			hclspec.NewLiteral("true"),
		),
		"default_config": hclspec.NewDefault(
			hclspec.NewAttr("default_config", "string", false),
			hclspec.NewLiteral("\""+lxc.GlobalConfigItem("lxc.default_config")+"\""),
		),
		"lxc_path": hclspec.NewAttr("lxc_path", "string", false),
		"network_mode": hclspec.NewDefault(
			hclspec.NewAttr("network_mode", "string", false),
			hclspec.NewLiteral("\"bridge\""),
		),
		// garbage collection options
		// default needed for both if the gc {...} block is not set and
		// if the default fields are missing
		"gc": hclspec.NewDefault(hclspec.NewBlock("gc", false, hclspec.NewObject(map[string]*hclspec.Spec{
			"container": hclspec.NewDefault(
				hclspec.NewAttr("container", "bool", false),
				hclspec.NewLiteral("true"),
			),
		})), hclspec.NewLiteral(`{
			container = true
		}`)),
	})

	// taskConfigSpec is the hcl specification for the driver config section of
	// a task within a job. It is returned in the TaskConfigSchema RPC
	taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		"template":       hclspec.NewAttr("template", "string", true),
		"distro":         hclspec.NewAttr("distro", "string", false),
		"release":        hclspec.NewAttr("release", "string", false),
		"arch":           hclspec.NewAttr("arch", "string", false),
		"image_variant":  hclspec.NewAttr("image_variant", "string", false),
		"image_server":   hclspec.NewAttr("image_server", "string", false),
		"gpg_key_id":     hclspec.NewAttr("gpg_key_id", "string", false),
		"gpg_key_server": hclspec.NewAttr("gpg_key_server", "string", false),
		"disable_gpg":    hclspec.NewAttr("disable_gpg", "string", false),
		"flush_cache":    hclspec.NewAttr("flush_cache", "string", false),
		"force_cache":    hclspec.NewAttr("force_cache", "string", false),
		"template_args":  hclspec.NewAttr("template_args", "list(string)", false),
		"log_level":      hclspec.NewAttr("log_level", "string", false),
		"verbosity":      hclspec.NewAttr("verbosity", "string", false),
		"volumes":        hclspec.NewAttr("volumes", "list(string)", false),
		"network_mode":   hclspec.NewAttr("network_mode", "string", false),
		"command":        hclspec.NewAttr("command", "list(string)", false),
		"environment":    hclspec.NewAttr("environment", "list(string)", false),
		"cgroup":	  hclspec.NewAttr("cgroup", "string", false),
		"portmap":        hclspec.NewAttr("portmap", "list(map(number))", false),
	})

	// capabilities is returned by the Capabilities RPC and indicates what
	// optional features this driver supports
	capabilities = &drivers.Capabilities{
		SendSignals: false,
		Exec:        false,
		FSIsolation: drivers.FSIsolationImage,
	}
)

// Driver is a driver for running LXC containers
type Driver struct {
	// eventer is used to handle multiplexing of TaskEvents calls such that an
	// event can be broadcast to all callers
	eventer *eventer.Eventer

	// config is the driver configuration set by the SetConfig RPC
	config *Config

	// nomadConfig is the client config from nomad
	nomadConfig *base.ClientDriverConfig

	// tasks is the in memory datastore mapping taskIDs to rawExecDriverHandles
	tasks *taskStore

	// ctx is the context for the driver. It is passed to other subsystems to
	// coordinate shutdown
	ctx context.Context

	// signalShutdown is called when the driver is shutting down and cancels the
	// ctx passed to any subsystems
	signalShutdown context.CancelFunc

	// logger will log to the Nomad agent
	logger hclog.Logger
}

// GCConfig is the driver GarbageCollection configuration
type GCConfig struct {
	Container bool `codec:"container"`
}

// Config is the driver configuration set by the SetConfig RPC call
type Config struct {
	// Enabled is set to true to enable the lxc driver
	Enabled bool `codec:"enabled"`

	AllowVolumes bool `codec:"volumes_enabled"`

	DefaultConfig string `codec:"default_config"`

	LXCPath string `codec:"lxc_path"`

	// default networking mode if not specified in task config
	NetworkMode string `codec:"network_mode"`

	GC GCConfig `codec:"gc"`
}

// TaskConfig is the driver configuration of a task within a job
type TaskConfig struct {
	Template             string         `codec:"template"`
	Distro               string         `codec:"distro"`
	Release              string         `codec:"release"`
	Arch                 string         `codec:"arch"`
	ImageVariant         string         `codec:"image_variant"`
	ImageServer          string         `codec:"image_server"`
	GPGKeyID             string         `codec:"gpg_key_id"`
	GPGKeyServer         string         `codec:"gpg_key_server"`
	DisableGPGValidation bool           `codec:"disable_gpg"`
	FlushCache           bool           `codec:"flush_cache"`
	ForceCache           bool           `codec:"force_cache"`
	TemplateArgs         []string       `codec:"template_args"`
	LogLevel             string         `codec:"log_level"`
	Verbosity            string         `codec:"verbosity"`
	Volumes              []string       `codec:"volumes"`
	NetworkMode          string         `codec:"network_mode"`
	DefaultConfig        string         `codec:"default_config"`
	Command              []string       `codec:"command"`
	Environment          []string       `codec:"environment"`
	Cgroup	             string         `codec:"cgroup"`
	PortMap              map[string]int `codec:"portmap"`
}

// TaskState is the state which is encoded in the handle returned in
// StartTask. This information is needed to rebuild the task state and handler
// during recovery.
type TaskState struct {
	TaskConfig    *drivers.TaskConfig
	ContainerName string
	StartedAt     time.Time
}

// NewLXCDriver returns a new DriverPlugin implementation
func NewLXCDriver(logger hclog.Logger) drivers.DriverPlugin {
	ctx, cancel := context.WithCancel(context.Background())
	logger = logger.Named(pluginName)
	return &Driver{
		eventer:        eventer.NewEventer(ctx, logger),
		config:         &Config{},
		tasks:          newTaskStore(),
		ctx:            ctx,
		signalShutdown: cancel,
		logger:         logger,
	}
}

func (d *Driver) PluginInfo() (*base.PluginInfoResponse, error) {
	return pluginInfo, nil
}

func (d *Driver) ConfigSchema() (*hclspec.Spec, error) {
	return configSpec, nil
}

func (d *Driver) SetConfig(cfg *base.Config) error {
	var config Config
	if len(cfg.PluginConfig) != 0 {
		if err := base.MsgPackDecode(cfg.PluginConfig, &config); err != nil {
			return err
		}
	}

	d.config = &config
	if cfg.AgentConfig != nil {
		d.nomadConfig = cfg.AgentConfig.Driver
	}

	return nil
}

func (d *Driver) Shutdown(ctx context.Context) error {
	d.signalShutdown()
	return nil
}

func (d *Driver) TaskConfigSchema() (*hclspec.Spec, error) {
	return taskConfigSpec, nil
}

func (d *Driver) Capabilities() (*drivers.Capabilities, error) {
	return capabilities, nil
}

func (d *Driver) Fingerprint(ctx context.Context) (<-chan *drivers.Fingerprint, error) {
	ch := make(chan *drivers.Fingerprint)
	go d.handleFingerprint(ctx, ch)
	return ch, nil
}

func (d *Driver) handleFingerprint(ctx context.Context, ch chan<- *drivers.Fingerprint) {
	defer close(ch)
	ticker := time.NewTimer(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			ticker.Reset(fingerprintPeriod)
			ch <- d.buildFingerprint()
		}
	}
}

func (d *Driver) buildFingerprint() *drivers.Fingerprint {
	var health drivers.HealthState
	var desc string
	attrs := map[string]*pstructs.Attribute{}

	lxcVersion := lxc.Version()

	if d.config.Enabled && lxcVersion != "" {
		health = drivers.HealthStateHealthy
		desc = "ready"
		attrs["driver.lxc"] = pstructs.NewBoolAttribute(true)
		attrs["driver.lxc.version"] = pstructs.NewStringAttribute(lxcVersion)
	} else {
		health = drivers.HealthStateUndetected
		desc = "disabled"
	}

	if d.config.AllowVolumes {
		attrs["driver.lxc.volumes.enabled"] = pstructs.NewBoolAttribute(true)
	}

	return &drivers.Fingerprint{
		Attributes:        attrs,
		Health:            health,
		HealthDescription: desc,
	}
}

func (d *Driver) RecoverTask(handle *drivers.TaskHandle) error {
	d.logger.Info("recover lxc task", "driver_cfg", hclog.Fmt("%+v", handle))
	if handle == nil {
		return fmt.Errorf("error: handle cannot be nil")
	}

	// COMPAT(0.10): pre 0.9 upgrade path check
	if handle.Version == 0 {
		return d.recoverPre09Task(handle)
	}

	if _, ok := d.tasks.Get(handle.Config.ID); ok {
		return nil
	}

	var taskState TaskState
	if err := handle.GetDriverState(&taskState); err != nil {
		return fmt.Errorf("failed to decode task state from handle: %v", err)
	}

	c, err := lxc.NewContainer(taskState.ContainerName, d.lxcPath())
	if err != nil {
		return fmt.Errorf("failed to create container ref: %v", err)
	}

	initPid := c.InitPid()
	h := &taskHandle{
		container:  c,
		initPid:    initPid,
		taskConfig: taskState.TaskConfig,
		procState:  drivers.TaskStateRunning,
		startedAt:  taskState.StartedAt,
		exitResult: &drivers.ExitResult{},
		logger:     d.logger,

		totalCpuStats:  stats.NewCpuStats(),
		userCpuStats:   stats.NewCpuStats(),
		systemCpuStats: stats.NewCpuStats(),
	}

	d.tasks.Set(taskState.TaskConfig.ID, h)

	go h.run()
	return nil
}

func (d *Driver) StartTask(cfg *drivers.TaskConfig) (*drivers.TaskHandle, *drivers.DriverNetwork, error) {
	if _, ok := d.tasks.Get(cfg.ID); ok {
		return nil, nil, fmt.Errorf("task with ID %q already started", cfg.ID)
	}

	var driverConfig TaskConfig
	if err := cfg.DecodeDriverConfig(&driverConfig); err != nil {
		return nil, nil, fmt.Errorf("failed to decode driver config: %v", err)
	}

	d.logger.Info("starting lxc task driver", "cfg", hclog.Fmt("%+v", cfg))
	d.logger.Info("starting lxc task", "driver_cfg", hclog.Fmt("%+v", driverConfig))
	handle := drivers.NewTaskHandle(taskHandleVersion)
	handle.Config = cfg

	c, err := d.initializeContainer(cfg, driverConfig)
	if err != nil {
		d.logger.Error("failed to initializeContainer", "error", err)
		return nil, nil, err
	}

	opt := toLXCCreateOptions(driverConfig)

	if err := c.Create(opt); err != nil {
		return nil, nil, nstructs.NewRecoverableError(err, true)
	}

	cleanup := func() {
		if c.Running() {
			if err := c.Stop(); err != nil {
				d.logger.Error("failed to Stop during clean up from an error in Start", "error", err)
			}
		}
		if err := c.Destroy(); err != nil {
			d.logger.Error("failed to Destroy during clean up from an error in Start", "error", err)
		}
	}

	if err := d.configureContainerNetwork(c, driverConfig); err != nil {
		cleanup()
		return nil, nil, err
	}

	if err := d.mountVolumes(c, cfg, driverConfig); err != nil {
		d.logger.Error("failed to mountVolumes", "error", err)
		cleanup()
		return nil, nil, err
	}

	if err := c.StartExecute(driverConfig.Command); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("unable to start container: err %v", err)
	}

	if err := d.setResourceLimits(c, cfg); err != nil {
		cleanup()
		return nil, nil, err
	}

	pid := c.InitPid()

	h := &taskHandle{
		container:  c,
		initPid:    pid,
		taskConfig: cfg,
		procState:  drivers.TaskStateRunning,
		startedAt:  time.Now().Round(time.Millisecond),
		logger:     d.logger,

		totalCpuStats:  stats.NewCpuStats(),
		userCpuStats:   stats.NewCpuStats(),
		systemCpuStats: stats.NewCpuStats(),
	}

	driverState := TaskState{
		ContainerName: c.Name(),
		TaskConfig:    cfg,
		StartedAt:     h.startedAt,
	}

	if err := handle.SetDriverState(&driverState); err != nil {
		d.logger.Error("failed to start task, error setting driver state", "error", err)
		cleanup()
		return nil, nil, fmt.Errorf("failed to set driver state: %v", err)
	}

	d.tasks.Set(cfg.ID, h)

	go h.run()

	return handle, nil, nil
}

func (d *Driver) WaitTask(ctx context.Context, taskID string) (<-chan *drivers.ExitResult, error) {
	d.logger.Info("wait lxc task", "driver_cfg", hclog.Fmt("%+v", taskID))
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	ch := make(chan *drivers.ExitResult)
	go d.handleWait(ctx, handle, ch)

	return ch, nil
}

func (d *Driver) handleWait(ctx context.Context, handle *taskHandle, ch chan *drivers.ExitResult) {
	defer close(ch)

	//
	// Wait for process completion by polling status from handler.
	// We cannot use the following alternatives:
	//   * Process.Wait() requires LXC container processes to be children
	//     of self process; but LXC runs container in separate PID hierarchy
	//     owned by PID 1.
	//   * lxc.Container.Wait() holds a write lock on container and prevents
	//     any other calls, including stats.
	//
	// Going with simplest approach of polling for handler to mark exit.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			s := handle.TaskStatus()
			if s.State == drivers.TaskStateExited {
				ch <- handle.exitResult
			}
		}
	}
}

func (d *Driver) StopTask(taskID string, timeout time.Duration, signal string) error {
	d.logger.Info("stop lxc task", "driver_cfg", hclog.Fmt("%+v", taskID))
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	if err := handle.shutdown(timeout); err != nil {
		d.logger.Info("stop lxc task", "driver_cfg", hclog.Fmt("failed, %+v", err))
		return fmt.Errorf("executor Shutdown failed: %v", err)
	}

	return nil
}

func (d *Driver) DestroyTask(taskID string, force bool) error {
	d.logger.Info("destory lxc task", "driver_cfg", hclog.Fmt("%+v", taskID))
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	if handle.IsRunning() && !force {
		d.logger.Info("destroy lxc task", "driver_cfg", hclog.Fmt("cannot destroy running task"))
		return fmt.Errorf("cannot destroy running task")
	}

	if handle.IsRunning() {
		// grace period is chosen arbitrary here
		if err := handle.shutdown(1 * time.Minute); err != nil {
			d.logger.Info("destory lxc task", "driver_cfg", hclog.Fmt("failed to stop %+v", err))
			handle.logger.Error("failed to destroy executor", "err", err)
		}
	}
	handle.logger.Info("Destroying container", "container", handle.container.Name())
	// delete the container itself
	if err := handle.container.Destroy(); err != nil {
		d.logger.Info("destory lxc task", "driver_cfg", hclog.Fmt("failed to delete %+v", err))
		handle.logger.Error("failed to destroy lxc container", "err", err)
	}
	// finally cleanup task map
	d.tasks.Delete(taskID)
	return nil
}

func (d *Driver) InspectTask(taskID string) (*drivers.TaskStatus, error) {
	d.logger.Info("inspect lxc task", "driver_cfg", hclog.Fmt("%+v", taskID))
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	return handle.TaskStatus(), nil
}

func (d *Driver) TaskStats(ctx context.Context, taskID string, interval time.Duration) (<-chan *drivers.TaskResourceUsage, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	return handle.stats(ctx, interval)
}

func (d *Driver) TaskEvents(ctx context.Context) (<-chan *drivers.TaskEvent, error) {
	return d.eventer.TaskEvents(ctx)
}

func (d *Driver) SignalTask(taskID string, signal string) error {
	return fmt.Errorf("LXC driver does not support signals")
}

func (d *Driver) ExecTask(taskID string, cmd []string, timeout time.Duration) (*drivers.ExecTaskResult, error) {
	return nil, fmt.Errorf("LXC driver does not support exec")
}
