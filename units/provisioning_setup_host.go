package units

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/evergreen-ci/evergreen"
	"github.com/evergreen-ci/evergreen/cloud"
	"github.com/evergreen-ci/evergreen/model/distro"
	"github.com/evergreen-ci/evergreen/model/event"
	"github.com/evergreen-ci/evergreen/model/host"
	"github.com/evergreen-ci/evergreen/model/user"
	"github.com/evergreen-ci/evergreen/util"
	"github.com/mongodb/amboy"
	"github.com/mongodb/amboy/dependency"
	"github.com/mongodb/amboy/job"
	"github.com/mongodb/amboy/registry"
	"github.com/mongodb/grip"
	"github.com/mongodb/grip/message"
	"github.com/pkg/errors"
)

const (
	provisionRetryLimit = 15
	setupHostJobName    = "provisioning-setup-host"
	scpTimeout          = time.Minute
)

func init() {
	registry.AddJobType(setupHostJobName, func() amboy.Job {
		return makeSetupHostJob()
	})
}

type setupHostJob struct {
	HostID   string `bson:"host_id" json:"host_id" yaml:"host_id"`
	job.Base `bson:"metadata" json:"metadata" yaml:"metadata"`

	host *host.Host
	env  evergreen.Environment
}

func makeSetupHostJob() *setupHostJob {
	j := &setupHostJob{
		Base: job.Base{
			JobType: amboy.JobType{
				Name:    setupHostJobName,
				Version: 0,
			},
		},
	}

	j.SetDependency(dependency.NewAlways())
	return j
}

func NewHostSetupJob(env evergreen.Environment, h host.Host, id string) amboy.Job {
	j := makeSetupHostJob()
	j.host = &h
	j.HostID = h.Id
	j.env = env
	j.SetPriority(1)
	j.SetID(fmt.Sprintf("%s.%s.%s", setupHostJobName, j.HostID, id))
	return j
}

func (j *setupHostJob) Run(ctx context.Context) {
	var err error
	defer j.MarkComplete()

	if j.host == nil {
		j.host, err = host.FindOneId(j.HostID)
		if err != nil {
			j.AddError(err)
			return
		}
		if j.host == nil {
			j.AddError(fmt.Errorf("could not find host %s for job %s", j.HostID, j.TaskID))
			return
		}
	}
	if j.host.Status == evergreen.HostRunning || j.host.Provisioned {
		grip.Info(message.Fields{
			"job":     j.ID(),
			"host":    j.host.Id,
			"message": "skipping setup because host is already set up",
		})
		return
	}

	if j.env == nil {
		j.env = evergreen.GetEnvironment()
	}
	defer j.tryRequeue(ctx)

	settings := j.env.Settings()

	j.AddError(j.setupHost(ctx, j.host, settings))
}

var (
	errIgnorableCreateHost = errors.New("host.create encountered internal error")
)

func (j *setupHostJob) setupHost(ctx context.Context, h *host.Host, settings *evergreen.Settings) error {
	grip.Info(message.Fields{
		"message": "attempting to setup host",
		"distro":  h.Distro.Id,
		"hostid":  h.Id,
		"DNS":     h.Host,
		"job":     j.ID(),
	})

	if err := ctx.Err(); err != nil {
		return errors.Wrapf(err, "hostinit canceled during setup for host %s", h.Id)
	}

	setupStartTime := time.Now()
	grip.Info(message.Fields{
		"message": "provisioning host",
		"job":     j.ID(),
		"distro":  h.Distro.Id,
		"hostid":  h.Id,
	})

	if err := j.provisionHost(ctx, h, settings); err != nil {
		event.LogHostProvisionError(h.Id)

		if h.Distro.BootstrapSettings.Method == distro.BootstrapMethodSSH {
			grip.Error(message.WrapError(j.host.DeleteJasperCredentials(ctx, j.env), message.Fields{
				"message":  "could not delete Jasper credentials after failed provision attempt",
				"host":     j.host.Id,
				"distro":   j.host.Distro.Id,
				"attempts": h.ProvisionAttempts,
				"job":      j.ID(),
			}))
		}

		grip.Error(message.WrapError(err, message.Fields{
			"message":  "provisioning host encountered error",
			"job":      j.ID(),
			"distro":   h.Distro.Id,
			"hostid":   h.Id,
			"attempts": h.ProvisionAttempts,
		}))
	}

	// ProvisionHost allows hosts to fail provisioning a few
	// times during host start up, to account for the fact
	// that hosts often need extra time to come up.
	//
	// In these cases, ProvisionHost returns a nil error but
	// does not change the host status.
	if h.Status == evergreen.HostProvisioning && !h.Provisioned {
		grip.Info(message.Fields{
			"attempts": h.ProvisionAttempts,
			"host_id":  h.Id,
			"job":      j.ID(),
			"message":  "retrying provisioning",
		})
		return nil
	}

	grip.Info(message.Fields{
		"message":  "successfully finished provisioning host",
		"hostid":   h.Id,
		"DNS":      h.Host,
		"distro":   h.Distro.Id,
		"job":      j.ID(),
		"attempts": h.ProvisionAttempts,
		"runtime":  time.Since(setupStartTime),
	})

	return nil
}

func (j *setupHostJob) setDNSName(ctx context.Context, host *host.Host, cloudMgr cloud.Manager, settings *evergreen.Settings) error {
	if host.Host != "" {
		return nil
	}

	// get the DNS name for the host
	hostDNS, err := cloudMgr.GetDNSName(ctx, host)
	if err != nil {
		return errors.Wrapf(err, "error checking DNS name for host %s", host.Id)
	}

	// sanity check for the host DNS name
	if hostDNS == "" {
		// DNS name not required if IP address set
		if host.IP != "" {
			return nil
		}
		return errors.Errorf("instance %s is running but not returning a DNS name or IP address", host.Id)
	}

	// update the host's DNS name
	if err = host.SetDNSName(hostDNS); err != nil {
		return errors.Wrapf(err, "error setting DNS name for host %s", host.Id)
	}

	return nil
}

// runHostSetup transfers the specified setup script for an individual host.
// Returns the output from running the script remotely, as well as any error
// that occurs. If the script exits with a non-zero exit code, the error will be
// non-nil.
func (j *setupHostJob) runHostSetup(ctx context.Context, targetHost *host.Host, settings *evergreen.Settings) error {
	// fetch the appropriate cloud provider for the host
	mgrOpts, err := cloud.GetManagerOptions(targetHost.Distro)
	if err != nil {
		return errors.Wrapf(err, "can't get ManagerOpts for '%s'", targetHost.Id)
	}
	cloudMgr, err := cloud.GetManager(ctx, j.env, mgrOpts)
	if err != nil {
		return errors.Wrapf(err,
			"failed to get cloud manager for host %s with provider %s",
			targetHost.Id, targetHost.Provider)
	}

	if err = j.setDNSName(ctx, targetHost, cloudMgr, settings); err != nil {
		return errors.Wrap(err, "error setting DNS name")
	}

	// run the function scheduled for when the host is up
	if err = cloudMgr.OnUp(ctx, targetHost); err != nil {
		err = errors.Wrapf(err, "OnUp callback failed for host %s", targetHost.Id)
		return err
	}

	switch targetHost.Distro.BootstrapSettings.Method {
	case distro.BootstrapMethodNone:
		return nil
	case distro.BootstrapMethodUserData:
		// Updating the host LCT prevents the agent monitor deploy job from
		// running. The agent monitor should be started by the user data script.
		grip.Error(message.WrapError(targetHost.UpdateLastCommunicated(), message.Fields{
			"message": "failed to update host's last communication time",
			"host":    targetHost.Id,
			"distro":  targetHost.Distro.Id,
			"job":     j.ID(),
		}))

		// Do not set the host to running - hosts bootstrapped with user data
		// are not considered done provisioning until user data has finished
		// running.
		if err = targetHost.SetProvisionedNotRunning(); err != nil {
			return errors.Wrapf(err, "error marking host %s as provisioned", targetHost.Id)
		}

		grip.Info(message.Fields{
			"message":                 "host successfully provisioned by app server, awaiting host to finish provisioning itself",
			"host":                    targetHost.Id,
			"distro":                  targetHost.Distro.Id,
			"provisioning":            targetHost.Distro.BootstrapSettings.Method,
			"provider":                targetHost.Provider,
			"attempts":                targetHost.ProvisionAttempts,
			"job":                     j.ID(),
			"provision_duration_secs": time.Since(targetHost.CreationTime).Seconds(),
		})
		return nil
	case distro.BootstrapMethodSSH:
		if err = setupJasper(ctx, j.env, settings, j.host); err != nil {
			grip.Error(message.WrapError(err, message.Fields{
				"message": "could not set up Jasper",
				"host":    j.host.Id,
				"distro":  j.host.Distro.Id,
				"job":     j.ID(),
			}))
			return errors.Wrapf(err, "error putting Jasper on host '%s'", targetHost.Id)
		}
		grip.Info(message.Fields{
			"message": "successfully fetched Jasper binary and started service",
			"host":    j.host.Id,
			"job":     j.ID(),
			"distro":  j.host.Distro.Id,
		})
	}

	// Do not copy setup scripts to task-spawned hosts
	if targetHost.SpawnOptions.SpawnedByTask {
		return nil
	}

	if targetHost.Distro.Setup != "" {
		scriptName := evergreen.SetupScriptName
		if targetHost.Distro.IsPowerShellSetup() {
			scriptName = evergreen.PowerShellSetupScriptName
		}
		err = j.copyScript(ctx, settings, targetHost, filepath.Join("~", scriptName), targetHost.Distro.Setup)
		if err != nil {
			return errors.Wrapf(err, "error copying setup script %s to host %s",
				scriptName, targetHost.Id)
		}
	}

	if targetHost.Distro.Teardown != "" {
		err = j.copyScript(ctx, settings, targetHost, filepath.Join("~", evergreen.TeardownScriptName), targetHost.Distro.Teardown)
		if err != nil {
			return errors.Wrapf(err, "error copying teardown script %s to host %s",
				evergreen.TeardownScriptName, targetHost.Id)
		}
	}

	return nil
}

// setupJasper sets up the Jasper service on the host by putting the credentials
// on the host, downloading the latest version of Jasper, and restarting the
// Jasper service.
func setupJasper(ctx context.Context, env evergreen.Environment, settings *evergreen.Settings, h *host.Host) error {
	sshOptions, err := h.GetSSHOptions(settings)
	if err != nil {
		return errors.Wrapf(err, "error getting ssh options for host %s", h.Id)
	}

	if err := setupServiceUser(ctx, env, settings, h, sshOptions); err != nil {
		return errors.Wrap(err, "error setting up service user")
	}

	if err := putJasperCredentials(ctx, env, settings, h, sshOptions); err != nil {
		return errors.Wrap(err, "error putting Jasper credentials on remote host")
	}

	if err := doFetchAndReinstallJasper(ctx, env, h, sshOptions); err != nil {
		return errors.Wrap(err, "error starting Jasper service on remote host")
	}

	return nil
}

// putJasperCredentials creates Jasper credentials for the host and puts the
// credentials file on the host.
func putJasperCredentials(ctx context.Context, env evergreen.Environment, settings *evergreen.Settings, h *host.Host, sshOptions []string) error {
	creds, err := h.GenerateJasperCredentials(ctx, env)
	if err != nil {
		return errors.Wrap(err, "could not generate Jasper credentials for host")
	}

	writeCmds, err := h.WriteJasperCredentialsFilesCommands(settings.Splunk, creds)
	if err != nil {
		return errors.Wrap(err, "could not get command to write Jasper credentials file")
	}

	grip.Info(message.Fields{
		"message": "putting Jasper credentials on host",
		"host":    h.Id,
		"distro":  h.Distro.Id,
	})

	ctx, cancel := context.WithTimeout(ctx, scpTimeout)
	defer cancel()

	if logs, err := h.RunSSHCommandLiterally(ctx, writeCmds, sshOptions); err != nil {
		return errors.Wrapf(err, "error copying credentials to remote machine: command returned %s", logs)
	}

	if err := h.SaveJasperCredentials(ctx, env, creds); err != nil {
		return errors.Wrap(err, "error saving credentials")
	}

	return nil
}

// setupServiceUser runs the SSH commands on the host that sets up the service
// user on the host.
func setupServiceUser(ctx context.Context, env evergreen.Environment, settings *evergreen.Settings, h *host.Host, sshOptions []string) error {
	if !h.Distro.IsWindows() {
		return nil
	}

	cmds, err := h.SetupServiceUserCommands()
	if err != nil {
		return errors.Wrap(err, "could not get command to set up service user")
	}

	path := filepath.Join(h.Distro.HomeDir(), "setup-user.ps1")
	if err := copyScript(ctx, env, settings, h, path, cmds); err != nil {
		return errors.Wrap(err, "error copying script to set up service user")
	}

	if logs, err := h.RunSSHCommand(ctx, fmt.Sprintf("powershell ./%s && rm -f ./%s", filepath.Base(path), filepath.Base(path)), sshOptions); err != nil {
		return errors.Wrapf(err, "error while setting up service user: command returned %s", logs)
	}

	return nil
}

// doFetchAndReinstallJasper runs the SSH command that downloads the latest
// Jasper binary and restarts the service.
func doFetchAndReinstallJasper(ctx context.Context, env evergreen.Environment, h *host.Host, sshOptions []string) error {
	cmd := h.FetchAndReinstallJasperCommands(env.Settings())
	if logs, err := h.RunSSHCommandLiterally(ctx, cmd, sshOptions); err != nil {
		return errors.Wrapf(err, "error while fetching Jasper binary and installing service on remote host: command returned '%s'", logs)
	}
	return nil
}

// copyScript writes a given script as file "name" to the target host. This works
// by creating a local copy of the script on the runner's machine, scping it over
// then removing the local copy.
func copyScript(ctx context.Context, env evergreen.Environment, settings *evergreen.Settings, h *host.Host, name, script string) error {
	// parse the hostname into the user, host and port
	startAt := time.Now()

	hostInfo, err := h.GetSSHInfo()
	if err != nil {
		return errors.WithStack(err)
	}

	user := h.Distro.User
	if hostInfo.User != "" {
		user = hostInfo.User
	}

	// create a temp file for the script
	file, err := ioutil.TempFile("", filepath.Base(name))
	if err != nil {
		return errors.Wrap(err, "error creating temporary script file")
	}
	if err = os.Chmod(file.Name(), 0700); err != nil {
		return errors.Wrap(err, "error setting file permissions")
	}
	defer func() {
		errCtx := message.Fields{
			"operation": "cleaning up after script copy",
			"file":      file.Name(),
			"distro":    h.Distro.Id,
			"host":      h.Host,
			"name":      name,
		}
		grip.Error(message.WrapError(file.Close(), errCtx))
		grip.Error(message.WrapError(os.Remove(file.Name()), errCtx))
		grip.Debug(message.Fields{
			"operation":     "copy script",
			"file":          file.Name(),
			"distro":        h.Distro.Id,
			"host":          h.Host,
			"name":          name,
			"duration_secs": time.Since(startAt).Seconds(),
		})
	}()

	expanded, err := expandScript(script, settings)
	if err != nil {
		return errors.Wrapf(err, "error expanding script for host %s", h.Id)
	}
	if _, err = io.WriteString(file, expanded); err != nil {
		return errors.Wrap(err, "error writing local script")
	}

	sshOptions, err := h.GetSSHOptions(settings)
	if err != nil {
		return errors.Wrapf(err, "error getting ssh options for host %s", h.Id)
	}

	scpCmdOut := &util.CappedWriter{
		Buffer:   &bytes.Buffer{},
		MaxBytes: 1024 * 1024,
	}
	scpArgs := buildScpCommand(file.Name(), name, hostInfo, user, sshOptions)

	scpCmd := env.JasperManager().CreateCommand(ctx).Add(scpArgs).
		RedirectErrorToOutput(true).SetOutputWriter(scpCmdOut)

	// run the command to scp the script with a timeout
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, scpTimeout)
	defer cancel()

	err = scpCmd.Run(ctx)

	return errors.Wrap(err, "error copying script to remote machine")
}

// copyScript writes a given script as file "name" to the target host. This works
// by creating a local copy of the script on the runner's machine, scping it over
// then removing the local copy.
func (j *setupHostJob) copyScript(ctx context.Context, settings *evergreen.Settings, target *host.Host, name, script string) error {
	// parse the hostname into the user, host and port
	startAt := time.Now()

	hostInfo, err := target.GetSSHInfo()
	if err != nil {
		return errors.WithStack(err)
	}

	user := target.Distro.User
	if hostInfo.User != "" {
		user = hostInfo.User
	}

	// create a temp file for the script
	file, err := ioutil.TempFile("", filepath.Base(name))
	if err != nil {
		return errors.Wrap(err, "error creating temporary script file")
	}
	if err = os.Chmod(file.Name(), 0700); err != nil {
		return errors.Wrap(err, "error setting file permissions")
	}
	defer func() {
		errCtx := message.Fields{
			"job":       j.ID(),
			"operation": "cleaning up after script copy",
			"file":      file.Name(),
			"distro":    target.Distro.Id,
			"host":      target.Host,
			"name":      name,
		}
		grip.Error(message.WrapError(file.Close(), errCtx))
		grip.Error(message.WrapError(os.Remove(file.Name()), errCtx))
		grip.Debug(message.Fields{
			"job":           j.ID(),
			"operation":     "copy script",
			"file":          file.Name(),
			"distro":        target.Distro.Id,
			"host":          target.Host,
			"name":          name,
			"duration_secs": time.Since(startAt).Seconds(),
		})
	}()

	expanded, err := expandScript(script, settings)
	if err != nil {
		return errors.Wrapf(err, "error expanding script for host %s", target.Id)
	}
	if _, err = io.WriteString(file, expanded); err != nil {
		return errors.Wrap(err, "error writing local script")
	}

	sshOptions, err := target.GetSSHOptions(settings)
	if err != nil {
		return errors.Wrapf(err, "error getting ssh options for host %v", target.Id)
	}

	scpCmdOut := &util.CappedWriter{
		Buffer:   &bytes.Buffer{},
		MaxBytes: 1024 * 1024,
	}
	scpArgs := buildScpCommand(file.Name(), name, hostInfo, user, sshOptions)

	scpCmd := j.env.JasperManager().CreateCommand(ctx).Add(scpArgs).
		RedirectErrorToOutput(true).SetOutputWriter(scpCmdOut)

	// run the command to scp the script with a timeout
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, scpTimeout)
	defer cancel()

	err = scpCmd.Run(ctx)

	grip.Notice(message.WrapError(err, message.Fields{
		"message": "problem copying script to host",
		"job":     j.ID(),
		"command": strings.Join(scpArgs, " "),
		"distro":  target.Distro.Id,
		"host":    target.Host,
		"output":  scpCmdOut.String(),
	}))

	return errors.Wrap(err, "error copying script to remote machine")
}

func buildScpCommand(src, dst string, info *util.StaticHostInfo, user string, opts []string) []string {
	return append(append([]string{"scp", "-vvv", "-P", info.Port}, opts...), src, fmt.Sprintf("%s@%s:%s", user, info.Hostname, dst))
}

// Build the setup script that will need to be run on the specified host.
func expandScript(s string, settings *evergreen.Settings) (string, error) {
	// replace expansions in the script
	exp := util.NewExpansions(settings.Expansions)
	script, err := exp.ExpandString(s)
	if err != nil {
		return "", errors.Wrap(err, "expansions error")
	}
	return script, err
}

// Provision the host, and update the database accordingly.
func (j *setupHostJob) provisionHost(ctx context.Context, h *host.Host, settings *evergreen.Settings) error {
	grip.Info(message.Fields{
		"job":     j.ID(),
		"host":    h.Id,
		"distro":  h.Distro.Id,
		"message": "setting up host",
	})

	incErr := h.IncProvisionAttempts()
	grip.Critical(message.WrapError(incErr, message.Fields{
		"job":           j.ID(),
		"host":          h.Id,
		"attempt_value": h.ProvisionAttempts,
		"distro":        h.Distro.Id,
		"operation":     "increment provisioning errors failed",
	}))

	err := j.runHostSetup(ctx, h, settings)
	if err != nil {
		if shouldRetryProvisioning(h) {
			return nil
		}

		event.LogProvisionFailed(h.Id, "")
		// mark the host's provisioning as failed
		grip.Error(message.WrapError(h.SetUnprovisioned(), message.Fields{
			"operation": "setting host unprovisioned",
			"attempts":  h.ProvisionAttempts,
			"distro":    h.Distro.Id,
			"job":       j.ID(),
			"host":      h.Id,
		}))

		return errors.Wrapf(err, "error initializing host %s", h.Id)
	}
	if h.Distro.BootstrapSettings.Method == distro.BootstrapMethodUserData {
		return nil
	}

	// If this is a spawn host
	if h.ProvisionOptions != nil && h.ProvisionOptions.LoadCLI {
		grip.Infof("Uploading client binary to host %s", h.Id)
		lcr, err := j.loadClient(ctx, h, settings)
		if err != nil {
			grip.Error(message.WrapError(err, message.Fields{
				"message": "failed to load client binary onto host",
				"job":     j.ID(),
				"host":    h.Id,
				"distro":  h.Distro.Id,
			}))

			grip.Error(message.WrapError(h.SetUnprovisioned(), message.Fields{
				"operation": "setting host unprovisioned",
				"job":       j.ID(),
				"distro":    h.Distro.Id,
				"host":      h.Id,
			}))
			return errors.Wrapf(err, "Failed to load client binary onto host %s: %+v", h.Id, err)
		}

		sshOptions, err := h.GetSSHOptions(settings)
		if err != nil {
			grip.Error(message.WrapError(h.SetUnprovisioned(), message.Fields{
				"operation": "setting host unprovisioned",
				"distro":    h.Distro.Id,
				"job":       j.ID(),
				"host":      h.Id,
			}))
			return errors.Wrapf(err, "Error getting ssh options for host %s", h.Id)
		}

		d, err := distro.FindOne(distro.ById(h.Distro.Id))
		if err != nil {
			grip.Error(message.WrapError(h.SetUnprovisioned(), message.Fields{
				"operation": "setting host unprovisioned",
				"distro":    h.Distro.Id,
				"job":       j.ID(),
				"host":      h.Id,
			}))
			return errors.Wrapf(err, "Error finding distro for host %s", h.Id)
		}
		h.Distro = d

		grip.Infof("Running setup script for spawn host %s", h.Id)
		// run the setup script with the agent
		if logs, err := h.RunSSHCommand(ctx, h.SetupCommand(), sshOptions); err != nil {
			grip.Error(message.WrapError(h.SetUnprovisioned(), message.Fields{
				"operation": "setting host unprovisioned",
				"host":      h.Id,
				"distro":    h.Distro.Id,
				"job":       j.ID(),
			}))
			event.LogProvisionFailed(h.Id, logs)
			return errors.Wrapf(err, "error running setup script on remote host: %s", logs)
		}

		if h.ProvisionOptions.OwnerId != "" && len(h.ProvisionOptions.TaskId) > 0 {
			grip.Info(message.Fields{
				"message": "fetching data for task on host",
				"task":    h.ProvisionOptions.TaskId,
				"distro":  h.Distro.Id,
				"host":    h.Id,
				"job":     j.ID(),
			})

			grip.Error(message.WrapError(j.fetchRemoteTaskData(ctx, h.ProvisionOptions.TaskId, lcr.BinaryPath, lcr.ConfigPath, h, settings),
				message.Fields{
					"message": "failed to fetch data onto host",
					"task":    h.ProvisionOptions.TaskId,
					"host":    h.Id,
					"job":     j.ID(),
				}))
		}
	}

	grip.Info(message.Fields{
		"message": "setup complete for host",
		"host":    h.Id,
		"job":     j.ID(),
		"distro":  h.Distro.Id,
	})

	// the setup was successful. update the host accordingly in the database
	if err := h.MarkAsProvisioned(); err != nil {
		return errors.Wrapf(err, "error marking host %s as provisioned", h.Id)
	}

	grip.Info(message.Fields{
		"host":                    h.Id,
		"distro":                  h.Distro.Id,
		"provider":                h.Provider,
		"attempts":                h.ProvisionAttempts,
		"job":                     j.ID(),
		"message":                 "host successfully provisioned",
		"provision_duration_secs": h.ProvisionTime.Sub(h.CreationTime).Seconds(),
	})

	return nil
}

// loadClientResult indicates the locations on a target host where the CLI binary and it's config
// file have been written to.
type loadClientResult struct {
	BinaryPath string
	ConfigPath string
}

// loadClient places the evergreen command line client on the host, places a copy of the user's
// settings onto the host, and makes the binary appear in the $PATH when the user logs in.
// If successful, returns an instance of loadClientResult which contains the paths where the
// binary and config file were written to.
func (j *setupHostJob) loadClient(ctx context.Context, target *host.Host, settings *evergreen.Settings) (*loadClientResult, error) {
	if target.ProvisionOptions == nil {
		return nil, errors.New("ProvisionOptions is nil")
	}
	if target.ProvisionOptions.OwnerId == "" {
		return nil, errors.New("OwnerId not set")
	}

	// get the information about the owner of the host
	owner, err := user.FindOne(user.ById(target.ProvisionOptions.OwnerId))
	if err != nil {
		return nil, errors.Wrapf(err, "couldn't fetch owner %v for host", target.ProvisionOptions.OwnerId)
	}

	// 1. mkdir the destination directory on the host,
	//    and modify ~/.profile so the target binary will be on the $PATH
	targetDir := "cli_bin"
	hostSSHInfo, err := target.GetSSHInfo()
	if err != nil {
		return nil, errors.Wrapf(err, "error parsing ssh info %s", target.Host)
	}

	sshOptions, err := target.GetSSHOptions(settings)
	if err != nil {
		return nil, errors.Wrapf(err, "Error getting ssh options for host %v", target.Id)
	}
	sshOptions = append(sshOptions, "-o", "UserKnownHostsFile=/dev/null")

	mkdirctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	output, err := target.RunSSHCommandLiterally(mkdirctx, fmt.Sprintf("mkdir -m 777 -p ~/%s && (echo 'export PATH=\"$PATH:~/%s\"' >> ~/.profile || true; echo 'export PATH=\"$PATH:~/%s\"' >> ~/.bash_profile || true)", targetDir, targetDir, targetDir), sshOptions)
	if err != nil {
		return nil, errors.Wrapf(err, "error running setup command for cli: %s", output)
	}

	// run the command to curl the agent
	curlctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	curlOut := &util.CappedWriter{
		Buffer:   &bytes.Buffer{},
		MaxBytes: 1024 * 1024,
	}

	curlcmd := j.env.JasperManager().CreateCommand(curlctx).Host(hostSSHInfo.Hostname).User(target.User).
		ExtendRemoteArgs("-p", hostSSHInfo.Port).ExtendRemoteArgs(sshOptions...).
		RedirectErrorToOutput(true).SetOutputWriter(curlOut).
		Append(target.CurlCommand(settings))

	if err = curlcmd.Run(curlctx); err != nil {
		return nil, errors.Wrapf(err, "error running curl command for cli, %s", curlOut.Buffer.String())
	}

	// 2. Write a settings file for the user that owns the host, and scp it to the directory
	outputStruct := struct {
		APIKey        string `json:"api_key"`
		APIServerHost string `json:"api_server_host"`
		UIServerHost  string `json:"ui_server_host"`
		User          string `json:"user"`
	}{
		APIKey:        owner.APIKey,
		APIServerHost: settings.ApiUrl + "/api",
		UIServerHost:  settings.Ui.Url,
		User:          owner.Id,
	}
	outputJSON, err := json.Marshal(outputStruct)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	tempFileName, err := util.WriteTempFile("", outputJSON)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer os.Remove(tempFileName)

	scpOut := &util.CappedWriter{
		Buffer:   &bytes.Buffer{},
		MaxBytes: 1024 * 1024,
	}

	scpArgs := buildScpCommand(tempFileName, filepath.Join("~", targetDir, ".evergreen.yml"), hostSSHInfo, target.User, sshOptions)
	scpYmlCommand := j.env.JasperManager().CreateCommand(ctx).Add(scpArgs).
		RedirectErrorToOutput(true).SetOutputWriter(scpOut)

	ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err = scpYmlCommand.Run(ctx); err != nil {
		return nil, errors.Wrapf(err, "error running SCP command for evergreen.yml, %v", scpOut.String())
	}

	return &loadClientResult{
		BinaryPath: filepath.Join("~", "evergreen"),
		ConfigPath: fmt.Sprintf("%s/.evergreen.yml", targetDir),
	}, nil
}

func (j *setupHostJob) fetchRemoteTaskData(ctx context.Context, taskId, cliPath, confPath string, target *host.Host, settings *evergreen.Settings) error {
	hostSSHInfo, err := target.GetSSHInfo()
	if err != nil {
		return errors.Wrapf(err, "error parsing ssh info %s", target.Host)
	}

	sshOptions, err := target.GetSSHOptions(settings)
	if err != nil {
		return errors.Wrapf(err, "Error getting ssh options for host %v", target.Id)
	}
	sshOptions = append(sshOptions, "-o", "UserKnownHostsFile=/dev/null")

	cmdOutput := &util.CappedWriter{
		Buffer:   &bytes.Buffer{},
		MaxBytes: 1024 * 1024,
	}
	fetchCmd := fmt.Sprintf("%s -c %s fetch -t %s --source --artifacts --dir='%s'", cliPath, confPath, taskId, target.Distro.WorkDir)

	makeShellCmd := j.env.JasperManager().CreateCommand(ctx).Host(hostSSHInfo.Hostname).User(target.User).
		ExtendRemoteArgs("-p", hostSSHInfo.Port).ExtendRemoteArgs(sshOptions...).
		RedirectErrorToOutput(true).SetOutputWriter(cmdOutput).
		Append(fetchCmd)

	// run the make shell command with a timeout
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	err = makeShellCmd.Run(ctx)

	grip.Error(message.WrapError(err, message.Fields{
		"message": fmt.Sprintf("fetch-artifacts-%s", taskId),
		"host":    hostSSHInfo.Hostname,
		"cmd":     fetchCmd,
		"job":     j.ID(),
		"output":  cmdOutput.Buffer.String(),
	}))

	return errors.WithStack(err)
}

func (j *setupHostJob) tryRequeue(ctx context.Context) {
	if shouldRetryProvisioning(j.host) && j.env.RemoteQueue().Started() {
		job := NewHostSetupJob(j.env, *j.host, fmt.Sprintf("attempt-%d", j.host.ProvisionAttempts))
		job.UpdateTimeInfo(amboy.JobTimeInfo{
			WaitUntil: time.Now().Add(time.Minute),
		})
		err := j.env.RemoteQueue().Put(ctx, job)
		grip.Critical(message.WrapError(err, message.Fields{
			"message":  "failed to requeue setup job",
			"host":     j.host.Id,
			"job":      j.ID(),
			"distro":   j.host.Distro.Id,
			"attempts": j.host.ProvisionAttempts,
		}))
		j.AddError(err)
	}
}

func shouldRetryProvisioning(h *host.Host) bool {
	return h.ProvisionAttempts <= provisionRetryLimit && h.Status == evergreen.HostProvisioning && !h.Provisioned
}
