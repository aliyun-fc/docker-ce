package daemon // import "github.com/docker/docker/daemon"

import (
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/container"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/system"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

var cgroupFilePaths = map[string]string{"cpu,cpuacct":"/sys/fs/cgroup/cpu,cpuacct/docker/%s",
	"net_cls,net_prio":"/sys/fs/cgroup/net_cls,net_prio/docker/%s",
	"cpuset":"/sys/fs/cgroup/cpuset/docker/%s",
	"freezer":"/sys/fs/cgroup/freezer/docker/%s",
	"memory":"/sys/fs/cgroup/memory/docker/%s",
	"systemd":"/sys/fs/cgroup/systemd/docker/%s",
	"net_cls":"/sys/fs/cgroup/net_cls/docker/%s",
	"blkio":"/sys/fs/cgroup/blkio/docker/%s",
	"cpu":"/sys/fs/cgroup/cpu/docker/%s",
	"cpuacct":"/sys/fs/cgroup/cpuacct/docker/%s",
	"devices":"/sys/fs/cgroup/devices/docker/%s",
	"hugetlb":"/sys/fs/cgroup/hugetlb/docker/%s",
	"net_prio":"/sys/fs/cgroup/net_prio/docker/%s",
	"perf_event":"/sys/fs/cgroup/perf_event/docker/%s",
	"pids":"/sys/fs/cgroup/pids/docker/%s"}

// eemovePaths iterates over the provided paths removing them.
// We trying to remove all paths five times with increasing delay between tries.
// If after all there are not removed cgroups - appropriate error will be
// returned.
func removePaths(paths map[string]string) (err error) {
	delay := 10 * time.Millisecond
	for i := 0; i < 5; i++ {
		if i != 0 {
			time.Sleep(delay)
			delay *= 2
		}
		for s, p := range paths {
			os.RemoveAll(p)
			//os.RemoveAll(p)
			// TODO: here probably should be logging
			_, err := os.Stat(p)
			// We need this strange way of checking cgroups existence because
			// RemoveAll almost always returns error, even on already removed
			// cgroups
			if os.IsNotExist(err) {
				delete(paths, s)
			}
		}
		if len(paths) == 0 {
			return nil
		}
	}
	return fmt.Errorf("Failed to remove paths: %v", paths)
}

// ContainerRm removes the container id from the filesystem. An error
// is returned if the container is not found, or if the remove
// fails. If the remove succeeds, the container name is released, and
// network links are removed.
func (daemon *Daemon) ContainerRm(name string, config *types.ContainerRmConfig) error {
	start := time.Now()
	container, err := daemon.GetContainer(name)
	if err != nil {
		return err
	}

	// Container state RemovalInProgress should be used to avoid races.
	if inProgress := container.SetRemovalInProgress(); inProgress {
		err := fmt.Errorf("removal of container %s is already in progress", name)
		return errdefs.Conflict(err)
	}
	defer container.ResetRemovalInProgress()

	// check if container wasn't deregistered by previous rm since Get
	if c := daemon.containers.Get(container.ID); c == nil {
		return nil
	}

	if config.RemoveLink {
		return daemon.rmLink(container, name)
	}

	err = daemon.cleanupContainer(container, config.ForceRemove, config.RemoveVolume)
	containerActions.WithValues("delete").UpdateSince(start)

	return err
}

func (daemon *Daemon) rmLink(container *container.Container, name string) error {
	if name[0] != '/' {
		name = "/" + name
	}
	parent, n := path.Split(name)
	if parent == "/" {
		return fmt.Errorf("Conflict, cannot remove the default name of the container")
	}

	parent = strings.TrimSuffix(parent, "/")
	pe, err := daemon.containersReplica.Snapshot().GetID(parent)
	if err != nil {
		return fmt.Errorf("Cannot get parent %s for name %s", parent, name)
	}

	daemon.releaseName(name)
	parentContainer, _ := daemon.GetContainer(pe)
	if parentContainer != nil {
		daemon.linkIndex.unlink(name, container, parentContainer)
		if err := daemon.updateNetwork(parentContainer); err != nil {
			logrus.Debugf("Could not update network to remove link %s: %v", n, err)
		}
	}
	return nil
}

// cleanupContainer unregisters a container from the daemon, stops stats
// collection and cleanly removes contents and metadata from the filesystem.
func (daemon *Daemon) cleanupContainer(container *container.Container, forceRemove, removeVolume bool) (err error) {
	if container.IsRunning() {
		if !forceRemove {
			state := container.StateString()
			procedure := "Stop the container before attempting removal or force remove"
			if state == "paused" {
				procedure = "Unpause and then " + strings.ToLower(procedure)
			}
			err := fmt.Errorf("You cannot remove a %s container %s. %s", state, container.ID, procedure)
			return errdefs.Conflict(err)
		}
		if err := daemon.Kill(container); err != nil {
			return fmt.Errorf("Could not kill running container %s, cannot remove - %v", container.ID, err)
		}
	}
	if !system.IsOSSupported(container.OS) {
		return fmt.Errorf("cannot remove %s: %s ", container.ID, system.ErrNotSupportedOperatingSystem)
	}

	// stop collection of stats for the container regardless
	// if stats are currently getting collected.
	daemon.statsCollector.StopCollection(container)

	if err = daemon.containerStop(container, 3); err != nil {
		return err
	}

	// Mark container dead. We don't want anybody to be restarting it.
	container.Lock()
	container.Dead = true

	// Save container state to disk. So that if error happens before
	// container meta file got removed from disk, then a restart of
	// docker should not make a dead container alive.
	if err := container.CheckpointTo(daemon.containersReplica); err != nil && !os.IsNotExist(err) {
		logrus.Errorf("Error saving dying container to disk: %v", err)
	}
	container.Unlock()

	// When container creation fails and `RWLayer` has not been created yet, we
	// do not call `ReleaseRWLayer`
	if container.RWLayer != nil {
		err := daemon.imageService.ReleaseLayer(container.RWLayer, container.OS)
		if err != nil {
			err = errors.Wrapf(err, "container %s", container.ID)
			container.SetRemovalError(err)
			return err
		}
		container.RWLayer = nil
	}

	if err := system.EnsureRemoveAll(container.Root); err != nil {
		e := errors.Wrapf(err, "unable to remove filesystem for %s", container.ID)
		container.SetRemovalError(e)
		return e
	}

	// destroy cgroup files of container
	cgroupPaths := make(map[string]string)
	for s, p := range cgroupFilePaths {
		cgroupPaths[s] = fmt.Sprintf(p, container.ID)
	}

	err = removePaths(cgroupPaths)
	if err != nil {
		return fmt.Errorf("Fail to Destroy cgroups of container %s, err %s", container.ID, err)
	}

	linkNames := daemon.linkIndex.delete(container)
	selinuxFreeLxcContexts(container.ProcessLabel)
	daemon.idIndex.Delete(container.ID)
	daemon.containers.Delete(container.ID)
	daemon.containersReplica.Delete(container)
	if e := daemon.removeMountPoints(container, removeVolume); e != nil {
		logrus.Error(e)
	}
	for _, name := range linkNames {
		daemon.releaseName(name)
	}
	container.SetRemoved()
	stateCtr.del(container.ID)

	daemon.LogContainerEvent(container, "destroy")
	return nil
}
