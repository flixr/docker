package service

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/context"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	mounttypes "github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/cli"
	"github.com/docker/docker/cli/command"
	"github.com/docker/docker/opts"
	runconfigopts "github.com/docker/docker/runconfig/opts"
	"github.com/docker/go-connections/nat"
	shlex "github.com/flynn-archive/go-shlex"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func newUpdateCommand(dockerCli *command.DockerCli) *cobra.Command {
	opts := newServiceOptions()

	cmd := &cobra.Command{
		Use:   "update [OPTIONS] SERVICE",
		Short: "Update a service",
		Args:  cli.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(dockerCli, cmd.Flags(), args[0])
		},
	}

	flags := cmd.Flags()
	flags.String("image", "", "Service image tag")
	flags.String("args", "", "Service command args")
	flags.Bool("rollback", false, "Rollback to previous specification")
	flags.Bool("force", false, "Force update even if no changes require it")
	addServiceFlags(cmd, opts)

	flags.Var(newListOptsVar(), flagEnvRemove, "Remove an environment variable")
	flags.Var(newListOptsVar(), flagGroupRemove, "Remove a previously added supplementary user group from the container")
	flags.Var(newListOptsVar(), flagLabelRemove, "Remove a label by its key")
	flags.Var(newListOptsVar(), flagContainerLabelRemove, "Remove a container label by its key")
	flags.Var(newListOptsVar(), flagMountRemove, "Remove a mount by its target path")
	flags.Var(newListOptsVar(), flagPublishRemove, "Remove a published port by its target port")
	flags.Var(newListOptsVar(), flagConstraintRemove, "Remove a constraint")
	flags.Var(&opts.labels, flagLabelAdd, "Add or update a service label")
	flags.Var(&opts.containerLabels, flagContainerLabelAdd, "Add or update a container label")
	flags.Var(&opts.env, flagEnvAdd, "Add or update an environment variable")
	flags.Var(&opts.mounts, flagMountAdd, "Add or update a mount on a service")
	flags.StringSliceVar(&opts.constraints, flagConstraintAdd, []string{}, "Add or update a placement constraint")
	flags.Var(&opts.endpoint.ports, flagPublishAdd, "Add or update a published port")
	flags.StringSliceVar(&opts.groups, flagGroupAdd, []string{}, "Add an additional supplementary user group to the container")
	return cmd
}

func newListOptsVar() *opts.ListOpts {
	return opts.NewListOptsRef(&[]string{}, nil)
}

func runUpdate(dockerCli *command.DockerCli, flags *pflag.FlagSet, serviceID string) error {
	apiClient := dockerCli.Client()
	ctx := context.Background()
	updateOpts := types.ServiceUpdateOptions{}

	service, _, err := apiClient.ServiceInspectWithRaw(ctx, serviceID)
	if err != nil {
		return err
	}

	rollback, err := flags.GetBool("rollback")
	if err != nil {
		return err
	}

	spec := &service.Spec
	if rollback {
		spec = service.PreviousSpec
		if spec == nil {
			return fmt.Errorf("service does not have a previous specification to roll back to")
		}
	}

	err = updateService(flags, spec)
	if err != nil {
		return err
	}

	// only send auth if flag was set
	sendAuth, err := flags.GetBool(flagRegistryAuth)
	if err != nil {
		return err
	}
	if sendAuth {
		// Retrieve encoded auth token from the image reference
		// This would be the old image if it didn't change in this update
		image := spec.TaskTemplate.ContainerSpec.Image
		encodedAuth, err := command.RetrieveAuthTokenFromImage(ctx, dockerCli, image)
		if err != nil {
			return err
		}
		updateOpts.EncodedRegistryAuth = encodedAuth
	} else if rollback {
		updateOpts.RegistryAuthFrom = types.RegistryAuthFromPreviousSpec
	} else {
		updateOpts.RegistryAuthFrom = types.RegistryAuthFromSpec
	}

	err = apiClient.ServiceUpdate(ctx, service.ID, service.Version, *spec, updateOpts)
	if err != nil {
		return err
	}

	fmt.Fprintf(dockerCli.Out(), "%s\n", serviceID)
	return nil
}

func updateService(flags *pflag.FlagSet, spec *swarm.ServiceSpec) error {
	updateString := func(flag string, field *string) {
		if flags.Changed(flag) {
			*field, _ = flags.GetString(flag)
		}
	}

	updateInt64Value := func(flag string, field *int64) {
		if flags.Changed(flag) {
			*field = flags.Lookup(flag).Value.(int64Value).Value()
		}
	}

	updateFloat32 := func(flag string, field *float32) {
		if flags.Changed(flag) {
			*field, _ = flags.GetFloat32(flag)
		}
	}

	updateDuration := func(flag string, field *time.Duration) {
		if flags.Changed(flag) {
			*field, _ = flags.GetDuration(flag)
		}
	}

	updateDurationOpt := func(flag string, field **time.Duration) {
		if flags.Changed(flag) {
			val := *flags.Lookup(flag).Value.(*DurationOpt).Value()
			*field = &val
		}
	}

	updateUint64 := func(flag string, field *uint64) {
		if flags.Changed(flag) {
			*field, _ = flags.GetUint64(flag)
		}
	}

	updateUint64Opt := func(flag string, field **uint64) {
		if flags.Changed(flag) {
			val := *flags.Lookup(flag).Value.(*Uint64Opt).Value()
			*field = &val
		}
	}

	cspec := &spec.TaskTemplate.ContainerSpec
	task := &spec.TaskTemplate

	taskResources := func() *swarm.ResourceRequirements {
		if task.Resources == nil {
			task.Resources = &swarm.ResourceRequirements{}
		}
		return task.Resources
	}

	updateLabels(flags, &spec.Labels)
	updateContainerLabels(flags, &cspec.Labels)
	updateString("image", &cspec.Image)
	updateStringToSlice(flags, "args", &cspec.Args)
	updateEnvironment(flags, &cspec.Env)
	updateString(flagWorkdir, &cspec.Dir)
	updateString(flagUser, &cspec.User)
	if err := updateMounts(flags, &cspec.Mounts); err != nil {
		return err
	}

	if flags.Changed(flagLimitCPU) || flags.Changed(flagLimitMemory) {
		taskResources().Limits = &swarm.Resources{}
		updateInt64Value(flagLimitCPU, &task.Resources.Limits.NanoCPUs)
		updateInt64Value(flagLimitMemory, &task.Resources.Limits.MemoryBytes)
	}
	if flags.Changed(flagReserveCPU) || flags.Changed(flagReserveMemory) {
		taskResources().Reservations = &swarm.Resources{}
		updateInt64Value(flagReserveCPU, &task.Resources.Reservations.NanoCPUs)
		updateInt64Value(flagReserveMemory, &task.Resources.Reservations.MemoryBytes)
	}

	updateDurationOpt(flagStopGracePeriod, &cspec.StopGracePeriod)

	if anyChanged(flags, flagRestartCondition, flagRestartDelay, flagRestartMaxAttempts, flagRestartWindow) {
		if task.RestartPolicy == nil {
			task.RestartPolicy = &swarm.RestartPolicy{}
		}

		if flags.Changed(flagRestartCondition) {
			value, _ := flags.GetString(flagRestartCondition)
			task.RestartPolicy.Condition = swarm.RestartPolicyCondition(value)
		}
		updateDurationOpt(flagRestartDelay, &task.RestartPolicy.Delay)
		updateUint64Opt(flagRestartMaxAttempts, &task.RestartPolicy.MaxAttempts)
		updateDurationOpt(flagRestartWindow, &task.RestartPolicy.Window)
	}

	if anyChanged(flags, flagConstraintAdd, flagConstraintRemove) {
		if task.Placement == nil {
			task.Placement = &swarm.Placement{}
		}
		updatePlacement(flags, task.Placement)
	}

	if err := updateReplicas(flags, &spec.Mode); err != nil {
		return err
	}

	if anyChanged(flags, flagUpdateParallelism, flagUpdateDelay, flagUpdateMonitor, flagUpdateFailureAction, flagUpdateMaxFailureRatio) {
		if spec.UpdateConfig == nil {
			spec.UpdateConfig = &swarm.UpdateConfig{}
		}
		updateUint64(flagUpdateParallelism, &spec.UpdateConfig.Parallelism)
		updateDuration(flagUpdateDelay, &spec.UpdateConfig.Delay)
		updateDuration(flagUpdateMonitor, &spec.UpdateConfig.Monitor)
		updateString(flagUpdateFailureAction, &spec.UpdateConfig.FailureAction)
		updateFloat32(flagUpdateMaxFailureRatio, &spec.UpdateConfig.MaxFailureRatio)
	}

	if flags.Changed(flagEndpointMode) {
		value, _ := flags.GetString(flagEndpointMode)
		if spec.EndpointSpec == nil {
			spec.EndpointSpec = &swarm.EndpointSpec{}
		}
		spec.EndpointSpec.Mode = swarm.ResolutionMode(value)
	}

	if anyChanged(flags, flagGroupAdd, flagGroupRemove) {
		if err := updateGroups(flags, &cspec.Groups); err != nil {
			return err
		}
	}

	if anyChanged(flags, flagPublishAdd, flagPublishRemove) {
		if spec.EndpointSpec == nil {
			spec.EndpointSpec = &swarm.EndpointSpec{}
		}
		if err := updatePorts(flags, &spec.EndpointSpec.Ports); err != nil {
			return err
		}
	}

	if err := updateLogDriver(flags, &spec.TaskTemplate); err != nil {
		return err
	}

	force, err := flags.GetBool("force")
	if err != nil {
		return err
	}

	if force {
		spec.TaskTemplate.ForceUpdate++
	}

	if err := updateHealthcheck(flags, cspec); err != nil {
		return err
	}

	if flags.Changed(flagTTY) {
		tty, err := flags.GetBool(flagTTY)
		if err != nil {
			return err
		}
		cspec.TTY = tty
	}

	return nil
}

func updateStringToSlice(flags *pflag.FlagSet, flag string, field *[]string) error {
	if !flags.Changed(flag) {
		return nil
	}

	value, _ := flags.GetString(flag)
	valueSlice, err := shlex.Split(value)
	*field = valueSlice
	return err
}

func anyChanged(flags *pflag.FlagSet, fields ...string) bool {
	for _, flag := range fields {
		if flags.Changed(flag) {
			return true
		}
	}
	return false
}

func updatePlacement(flags *pflag.FlagSet, placement *swarm.Placement) {
	field, _ := flags.GetStringSlice(flagConstraintAdd)
	placement.Constraints = append(placement.Constraints, field...)

	toRemove := buildToRemoveSet(flags, flagConstraintRemove)
	placement.Constraints = removeItems(placement.Constraints, toRemove, itemKey)
}

func updateContainerLabels(flags *pflag.FlagSet, field *map[string]string) {
	if flags.Changed(flagContainerLabelAdd) {
		if *field == nil {
			*field = map[string]string{}
		}

		values := flags.Lookup(flagContainerLabelAdd).Value.(*opts.ListOpts).GetAll()
		for key, value := range runconfigopts.ConvertKVStringsToMap(values) {
			(*field)[key] = value
		}
	}

	if *field != nil && flags.Changed(flagContainerLabelRemove) {
		toRemove := flags.Lookup(flagContainerLabelRemove).Value.(*opts.ListOpts).GetAll()
		for _, label := range toRemove {
			delete(*field, label)
		}
	}
}

func updateLabels(flags *pflag.FlagSet, field *map[string]string) {
	if flags.Changed(flagLabelAdd) {
		if *field == nil {
			*field = map[string]string{}
		}

		values := flags.Lookup(flagLabelAdd).Value.(*opts.ListOpts).GetAll()
		for key, value := range runconfigopts.ConvertKVStringsToMap(values) {
			(*field)[key] = value
		}
	}

	if *field != nil && flags.Changed(flagLabelRemove) {
		toRemove := flags.Lookup(flagLabelRemove).Value.(*opts.ListOpts).GetAll()
		for _, label := range toRemove {
			delete(*field, label)
		}
	}
}

func updateEnvironment(flags *pflag.FlagSet, field *[]string) {
	envSet := map[string]string{}
	for _, v := range *field {
		envSet[envKey(v)] = v
	}
	if flags.Changed(flagEnvAdd) {
		value := flags.Lookup(flagEnvAdd).Value.(*opts.ListOpts)
		for _, v := range value.GetAll() {
			envSet[envKey(v)] = v
		}
	}

	*field = []string{}
	for _, v := range envSet {
		*field = append(*field, v)
	}

	toRemove := buildToRemoveSet(flags, flagEnvRemove)
	*field = removeItems(*field, toRemove, envKey)
}

func envKey(value string) string {
	kv := strings.SplitN(value, "=", 2)
	return kv[0]
}

func itemKey(value string) string {
	return value
}

func buildToRemoveSet(flags *pflag.FlagSet, flag string) map[string]struct{} {
	var empty struct{}
	toRemove := make(map[string]struct{})

	if !flags.Changed(flag) {
		return toRemove
	}

	toRemoveSlice := flags.Lookup(flag).Value.(*opts.ListOpts).GetAll()
	for _, key := range toRemoveSlice {
		toRemove[key] = empty
	}
	return toRemove
}

func removeItems(
	seq []string,
	toRemove map[string]struct{},
	keyFunc func(string) string,
) []string {
	newSeq := []string{}
	for _, item := range seq {
		if _, exists := toRemove[keyFunc(item)]; !exists {
			newSeq = append(newSeq, item)
		}
	}
	return newSeq
}

type byMountSource []mounttypes.Mount

func (m byMountSource) Len() int      { return len(m) }
func (m byMountSource) Swap(i, j int) { m[i], m[j] = m[j], m[i] }
func (m byMountSource) Less(i, j int) bool {
	a, b := m[i], m[j]

	if a.Source == b.Source {
		return a.Target < b.Target
	}

	return a.Source < b.Source
}

func updateMounts(flags *pflag.FlagSet, mounts *[]mounttypes.Mount) error {

	mountsByTarget := map[string]mounttypes.Mount{}

	if flags.Changed(flagMountAdd) {
		values := flags.Lookup(flagMountAdd).Value.(*opts.MountOpt).Value()
		for _, mount := range values {
			if _, ok := mountsByTarget[mount.Target]; ok {
				return fmt.Errorf("duplicate mount target")
			}
			mountsByTarget[mount.Target] = mount
		}
	}

	// Add old list of mount points minus updated one.
	for _, mount := range *mounts {
		if _, ok := mountsByTarget[mount.Target]; !ok {
			mountsByTarget[mount.Target] = mount
		}
	}

	newMounts := []mounttypes.Mount{}

	toRemove := buildToRemoveSet(flags, flagMountRemove)

	for _, mount := range mountsByTarget {
		if _, exists := toRemove[mount.Target]; !exists {
			newMounts = append(newMounts, mount)
		}
	}
	sort.Sort(byMountSource(newMounts))
	*mounts = newMounts
	return nil
}

func updateGroups(flags *pflag.FlagSet, groups *[]string) error {
	if flags.Changed(flagGroupAdd) {
		values, err := flags.GetStringSlice(flagGroupAdd)
		if err != nil {
			return err
		}
		*groups = append(*groups, values...)
	}
	toRemove := buildToRemoveSet(flags, flagGroupRemove)

	newGroups := []string{}
	for _, group := range *groups {
		if _, exists := toRemove[group]; !exists {
			newGroups = append(newGroups, group)
		}
	}
	// Sort so that result is predictable.
	sort.Strings(newGroups)

	*groups = newGroups
	return nil
}

type byPortConfig []swarm.PortConfig

func (r byPortConfig) Len() int      { return len(r) }
func (r byPortConfig) Swap(i, j int) { r[i], r[j] = r[j], r[i] }
func (r byPortConfig) Less(i, j int) bool {
	// We convert PortConfig into `port/protocol`, e.g., `80/tcp`
	// In updatePorts we already filter out with map so there is duplicate entries
	return portConfigToString(&r[i]) < portConfigToString(&r[j])
}

func portConfigToString(portConfig *swarm.PortConfig) string {
	protocol := portConfig.Protocol
	if protocol == "" {
		protocol = "tcp"
	}
	return fmt.Sprintf("%v/%s", portConfig.PublishedPort, protocol)
}

func updatePorts(flags *pflag.FlagSet, portConfig *[]swarm.PortConfig) error {
	// The key of the map is `port/protocol`, e.g., `80/tcp`
	portSet := map[string]swarm.PortConfig{}
	// Check to see if there are any conflict in flags.
	if flags.Changed(flagPublishAdd) {
		values := flags.Lookup(flagPublishAdd).Value.(*opts.ListOpts).GetAll()
		ports, portBindings, _ := nat.ParsePortSpecs(values)

		for port := range ports {
			newConfigs := convertPortToPortConfig(port, portBindings)
			for _, entry := range newConfigs {
				if v, ok := portSet[portConfigToString(&entry)]; ok && v != entry {
					return fmt.Errorf("conflicting port mapping between %v:%v/%s and %v:%v/%s", entry.PublishedPort, entry.TargetPort, entry.Protocol, v.PublishedPort, v.TargetPort, v.Protocol)
				}
				portSet[portConfigToString(&entry)] = entry
			}
		}
	}

	// Override previous PortConfig in service if there is any duplicate
	for _, entry := range *portConfig {
		if _, ok := portSet[portConfigToString(&entry)]; !ok {
			portSet[portConfigToString(&entry)] = entry
		}
	}

	toRemove := flags.Lookup(flagPublishRemove).Value.(*opts.ListOpts).GetAll()
	newPorts := []swarm.PortConfig{}
portLoop:
	for _, port := range portSet {
		for _, rawTargetPort := range toRemove {
			targetPort := nat.Port(rawTargetPort)
			if equalPort(targetPort, port) {
				continue portLoop
			}
		}
		newPorts = append(newPorts, port)
	}
	// Sort the PortConfig to avoid unnecessary updates
	sort.Sort(byPortConfig(newPorts))
	*portConfig = newPorts
	return nil
}

func equalPort(targetPort nat.Port, port swarm.PortConfig) bool {
	return (string(port.Protocol) == targetPort.Proto() &&
		port.TargetPort == uint32(targetPort.Int()))
}

func updateReplicas(flags *pflag.FlagSet, serviceMode *swarm.ServiceMode) error {
	if !flags.Changed(flagReplicas) {
		return nil
	}

	if serviceMode == nil || serviceMode.Replicated == nil {
		return fmt.Errorf("replicas can only be used with replicated mode")
	}
	serviceMode.Replicated.Replicas = flags.Lookup(flagReplicas).Value.(*Uint64Opt).Value()
	return nil
}

// updateLogDriver updates the log driver only if the log driver flag is set.
// All options will be replaced with those provided on the command line.
func updateLogDriver(flags *pflag.FlagSet, taskTemplate *swarm.TaskSpec) error {
	if !flags.Changed(flagLogDriver) {
		return nil
	}

	name, err := flags.GetString(flagLogDriver)
	if err != nil {
		return err
	}

	if name == "" {
		return nil
	}

	taskTemplate.LogDriver = &swarm.Driver{
		Name:    name,
		Options: runconfigopts.ConvertKVStringsToMap(flags.Lookup(flagLogOpt).Value.(*opts.ListOpts).GetAll()),
	}

	return nil
}

func updateHealthcheck(flags *pflag.FlagSet, containerSpec *swarm.ContainerSpec) error {
	if !anyChanged(flags, flagNoHealthcheck, flagHealthCmd, flagHealthInterval, flagHealthRetries, flagHealthTimeout) {
		return nil
	}
	if containerSpec.Healthcheck == nil {
		containerSpec.Healthcheck = &container.HealthConfig{}
	}
	noHealthcheck, err := flags.GetBool(flagNoHealthcheck)
	if err != nil {
		return err
	}
	if noHealthcheck {
		if !anyChanged(flags, flagHealthCmd, flagHealthInterval, flagHealthRetries, flagHealthTimeout) {
			containerSpec.Healthcheck = &container.HealthConfig{
				Test: []string{"NONE"},
			}
			return nil
		}
		return fmt.Errorf("--%s conflicts with --health-* options", flagNoHealthcheck)
	}
	if len(containerSpec.Healthcheck.Test) > 0 && containerSpec.Healthcheck.Test[0] == "NONE" {
		containerSpec.Healthcheck.Test = nil
	}
	if flags.Changed(flagHealthInterval) {
		val := *flags.Lookup(flagHealthInterval).Value.(*PositiveDurationOpt).Value()
		containerSpec.Healthcheck.Interval = val
	}
	if flags.Changed(flagHealthTimeout) {
		val := *flags.Lookup(flagHealthTimeout).Value.(*PositiveDurationOpt).Value()
		containerSpec.Healthcheck.Timeout = val
	}
	if flags.Changed(flagHealthRetries) {
		containerSpec.Healthcheck.Retries, _ = flags.GetInt(flagHealthRetries)
	}
	if flags.Changed(flagHealthCmd) {
		cmd, _ := flags.GetString(flagHealthCmd)
		if cmd != "" {
			containerSpec.Healthcheck.Test = []string{"CMD-SHELL", cmd}
		} else {
			containerSpec.Healthcheck.Test = nil
		}
	}
	return nil
}
