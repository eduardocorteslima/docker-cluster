// Copyright 2014 docker-cluster authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cluster

import (
	"errors"
	"fmt"
	"sync"

	"github.com/fsouza/go-dockerclient"
	"github.com/tsuru/docker-cluster/log"
)

// CreateContainer creates a container in the specified node. If no node is
// specified, it will create the container in a node selected by the scheduler.
//
// It returns the container, or an error, in case of failures.
func (c *Cluster) CreateContainer(opts docker.CreateContainerOptions, nodes ...string) (string, *docker.Container, error) {
	return c.CreateContainerSchedulerOpts(opts, nil, nodes...)
}

// Similar to CreateContainer but allows arbritary options to be passed to
// the scheduler.
func (c *Cluster) CreateContainerSchedulerOpts(opts docker.CreateContainerOptions, schedulerOpts SchedulerOptions, nodes ...string) (string, *docker.Container, error) {
	var (
		addr      string
		container *docker.Container
		err       error
	)
	useScheduler := len(nodes) == 0
	maxTries := 5
	for ; maxTries > 0; maxTries-- {
		if useScheduler {
			node, scheduleErr := c.scheduler.Schedule(c, opts, schedulerOpts)
			if scheduleErr != nil {
				if err != nil {
					scheduleErr = fmt.Errorf("Error in scheduler after previous errors (%s) trying to create container: %s", err.Error(), scheduleErr.Error())
				}
				return addr, nil, scheduleErr
			}
			addr = node.Address
		} else {
			addr = nodes[0]
		}
		if addr == "" {
			return addr, nil, errors.New("CreateContainer needs a non empty node addr")
		}
		container, err = c.createContainerInNode(opts, addr)
		if err == nil {
			c.handleNodeSuccess(addr)
			break
		} else {
			log.Errorf("Error trying to create container in node %q: %s. Trying again in another node...", addr, err.Error())
			c.handleNodeError(addr, err)
			if !useScheduler {
				return addr, nil, err
			}
		}
	}
	if err != nil {
		return addr, nil, fmt.Errorf("CreateContainer: maximum number of tries exceeded, last error: %s", err.Error())
	}
	err = c.storage().StoreContainer(container.ID, addr)
	return addr, container, err
}

func (c *Cluster) createContainerInNode(opts docker.CreateContainerOptions, nodeAddress string) (*docker.Container, error) {
	registryServer, _ := parseImageRegistry(opts.Config.Image)
	if registryServer != "" {
		err := c.PullImage(docker.PullImageOptions{
			Repository: opts.Config.Image,
		}, docker.AuthConfiguration{}, nodeAddress)
		if err != nil {
			return nil, fmt.Errorf("Error trying to pull image in node %q: %s", nodeAddress, err.Error())
		}
	}
	node, err := c.getNodeByAddr(nodeAddress)
	if err != nil {
		return nil, err
	}
	return node.CreateContainer(opts)
}

// InspectContainer returns information about a container by its ID, getting
// the information from the right node.
func (c *Cluster) InspectContainer(id string) (*docker.Container, error) {
	node, err := c.getNodeForContainer(id)
	if err != nil {
		return nil, err
	}
	return node.InspectContainer(id)
}

// KillContainer kills a container, returning an error in case of failure.
func (c *Cluster) KillContainer(opts docker.KillContainerOptions) error {
	node, err := c.getNodeForContainer(opts.ID)
	if err != nil {
		return err
	}
	return node.KillContainer(opts)
}

// ListContainers returns a slice of all containers in the cluster matching the
// given criteria.
func (c *Cluster) ListContainers(opts docker.ListContainersOptions) ([]docker.APIContainers, error) {
	nodes, err := c.Nodes()
	if err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	result := make(chan []docker.APIContainers, len(nodes))
	errs := make(chan error, len(nodes))
	for _, n := range nodes {
		wg.Add(1)
		client, _ := c.getNodeByAddr(n.Address)
		go func(n node) {
			defer wg.Done()
			if containers, err := n.ListContainers(opts); err != nil {
				errs <- err
			} else {
				result <- containers
			}
		}(client)
	}
	wg.Wait()
	var group []docker.APIContainers
	for {
		select {
		case containers := <-result:
			group = append(group, containers...)
		case err = <-errs:
		default:
			return group, err
		}
	}
}

// RemoveContainer removes a container from the cluster.
func (c *Cluster) RemoveContainer(opts docker.RemoveContainerOptions) error {
	return c.removeFromStorage(opts)
}

func (c *Cluster) removeFromStorage(opts docker.RemoveContainerOptions) error {
	node, err := c.getNodeForContainer(opts.ID)
	if err != nil {
		return err
	}
	err = node.RemoveContainer(opts)
	if err != nil {
		_, isNoSuchContainer := err.(*docker.NoSuchContainer)
		if !isNoSuchContainer {
			return err
		}
	}
	return c.storage().RemoveContainer(opts.ID)
}

func (c *Cluster) StartContainer(id string, hostConfig *docker.HostConfig) error {
	node, err := c.getNodeForContainer(id)
	if err != nil {
		return err
	}
	return node.StartContainer(id, hostConfig)
}

// StopContainer stops a container, killing it after the given timeout, if it
// fails to stop nicely.
func (c *Cluster) StopContainer(id string, timeout uint) error {
	node, err := c.getNodeForContainer(id)
	if err != nil {
		return err
	}
	return node.StopContainer(id, timeout)
}

// RestartContainer restarts a container, killing it after the given timeout,
// if it fails to stop nicely.
func (c *Cluster) RestartContainer(id string, timeout uint) error {
	node, err := c.getNodeForContainer(id)
	if err != nil {
		return err
	}
	return node.RestartContainer(id, timeout)
}

// PauseContainer changes the container to the paused state.
func (c *Cluster) PauseContainer(id string) error {
	node, err := c.getNodeForContainer(id)
	if err != nil {
		return err
	}
	return node.PauseContainer(id)
}

// UnpauseContainer removes the container from the paused state.
func (c *Cluster) UnpauseContainer(id string) error {
	node, err := c.getNodeForContainer(id)
	if err != nil {
		return err
	}
	return node.UnpauseContainer(id)
}

// WaitContainer blocks until the given container stops, returning the exit
// code of the container command.
func (c *Cluster) WaitContainer(id string) (int, error) {
	node, err := c.getNodeForContainer(id)
	if err != nil {
		return -1, err
	}
	return node.WaitContainer(id)
}

// AttachToContainer attaches to a container, using the given options.
func (c *Cluster) AttachToContainer(opts docker.AttachToContainerOptions) error {
	node, err := c.getNodeForContainer(opts.Container)
	if err != nil {
		return err
	}
	return node.AttachToContainer(opts)
}

// Logs retrieves the logs of the specified container.
func (c *Cluster) Logs(opts docker.LogsOptions) error {
	node, err := c.getNodeForContainer(opts.Container)
	if err != nil {
		return err
	}
	return node.Logs(opts)
}

// CommitContainer commits a container and returns the image id.
func (c *Cluster) CommitContainer(opts docker.CommitContainerOptions) (*docker.Image, error) {
	node, err := c.getNodeForContainer(opts.Container)
	if err != nil {
		return nil, err
	}
	image, err := node.CommitContainer(opts)
	if err != nil {
		return nil, err
	}
	key := opts.Repository
	if key != "" {
		err = c.storage().StoreImage(key, image.ID, node.addr)
		if err != nil {
			return nil, err
		}
	}
	return image, nil
}

// ExportContainer exports a container as a tar and writes
// the result in out.
func (c *Cluster) ExportContainer(opts docker.ExportContainerOptions) error {
	node, err := c.getNodeForContainer(opts.ID)
	if err != nil {
		return err
	}
	return node.ExportContainer(opts)
}

func (c *Cluster) getNodeForContainer(container string) (node, error) {
	return c.getNode(func(s Storage) (string, error) {
		return s.RetrieveContainer(container)
	})
}
