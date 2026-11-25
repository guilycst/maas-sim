package driver

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

const (
	ownerLabel = "io.maas-sim.instance"
	leaseImage = "busybox:1.37"
	seedPath   = "/var/lib/cloud/seed/nocloud"
)

type DockerOptions struct {
	NetworkName     string
	Subnet          netip.Prefix
	Image           string
	Privileged      bool
	CgroupNamespace string
	Binds           []string
	Tmpfs           map[string]string
}

type DockerDriver struct {
	client      *client.Client
	network     Network
	networkID   string
	networkName string
	prefix      string
	options     DockerOptions
	ownsNetwork bool
}

func NewDocker(ctx context.Context, options DockerOptions) (*DockerDriver, error) {
	if options.Image == "" {
		return nil, errors.New("docker mode requires a node image")
	}
	if options.Subnet.IsValid() && !options.Subnet.Addr().Is4() {
		return nil, errors.New("docker subnet must be IPv4")
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}

	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		cli.Close()
		return nil, err
	}
	d := &DockerDriver{
		client: cli, networkName: options.NetworkName, options: options, prefix: fmt.Sprintf("maas-sim-%x", nonce),
	}
	if err := d.prepare(ctx); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return nil, errors.Join(err, d.Cleanup(cleanupCtx), d.Close())
	}
	return d, nil
}

func (d *DockerDriver) prepare(ctx context.Context) error {
	current, err := d.client.NetworkInspect(ctx, d.networkName, network.InspectOptions{})
	if errdefs.IsNotFound(err) {
		options := network.CreateOptions{Driver: "bridge", Labels: d.labels()}
		if d.options.Subnet.IsValid() {
			subnet := d.options.Subnet.Masked()
			options.IPAM = &network.IPAM{Config: []network.IPAMConfig{{
				Subnet: subnet.String(), Gateway: subnet.Addr().Next().String(),
			}}}
		}
		created, createErr := d.client.NetworkCreate(ctx, d.networkName, options)
		if createErr != nil {
			return fmt.Errorf("create docker network: %w", createErr)
		}
		d.ownsNetwork, d.networkID = true, created.ID
		current, err = d.client.NetworkInspect(ctx, created.ID, network.InspectOptions{})
	}
	if err != nil {
		return fmt.Errorf("inspect docker network: %w", err)
	}
	if current.Driver != "bridge" {
		return fmt.Errorf("docker network %q must use the bridge driver", d.networkName)
	}
	if current.Options["com.docker.network.bridge.default_bridge"] == "true" {
		return fmt.Errorf("docker network %q must be a user-defined bridge", d.networkName)
	}
	d.networkID = current.ID
	for _, config := range current.IPAM.Config {
		subnet, err := netip.ParsePrefix(config.Subnet)
		if err != nil || !subnet.Addr().Is4() {
			continue
		}
		subnet = subnet.Masked()
		gateway := subnet.Addr().Next()
		if config.Gateway != "" {
			gateway, err = netip.ParseAddr(config.Gateway)
			if err != nil {
				continue
			}
		}
		// Docker can omit the automatically selected gateway from IPAM.Config.
		if !subnet.Contains(gateway) {
			continue
		}
		d.network = Network{Subnet: subnet, Gateway: gateway}
		break
	}
	if !d.network.Subnet.IsValid() {
		return fmt.Errorf("docker network %q has no IPv4 subnet and gateway", d.networkName)
	}

	if d.options.Subnet.IsValid() && d.network.Subnet != d.options.Subnet.Masked() {
		return fmt.Errorf("docker network %q has subnet %s, expected %s", d.networkName, d.network.Subnet, d.options.Subnet)
	}
	return d.ensureImage(ctx, d.options.Image)
}

func (d *DockerDriver) ensureImage(ctx context.Context, ref string) error {
	_, err := d.client.ImageInspect(ctx, ref)
	if err == nil {
		return nil
	}
	if !errdefs.IsNotFound(err) {
		return fmt.Errorf("inspect image %s: %w", ref, err)
	}
	stream, err := d.client.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", ref, err)
	}
	defer stream.Close()
	return readDockerProgress(stream)
}

func readDockerProgress(stream io.Reader) error {
	decoder := json.NewDecoder(stream)
	var tail string
	for {
		var message struct {
			Error  string `json:"error"`
			Stream string `json:"stream"`
		}
		err := decoder.Decode(&message)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if message.Error != "" {
			return fmt.Errorf("%s: %s", message.Error, strings.TrimSpace(tail))
		}
		tail += message.Stream
		if len(tail) > 2048 {
			tail = tail[len(tail)-2048:]
		}
	}
}

func (d *DockerDriver) labels() map[string]string {
	return map[string]string{ownerLabel: d.prefix}
}

func (d *DockerDriver) Network() Network { return d.network }

func (d *DockerDriver) Close() error { return d.client.Close() }

func (d *DockerDriver) Deploy(ctx context.Context, node Node) (_ Instance, deployErr error) {
	created, err := d.client.ContainerCreate(ctx, &container.Config{
		Image: d.options.Image, Hostname: node.Hostname, Labels: d.labels(),
		Volumes: map[string]struct{}{seedPath: {}},
	}, &container.HostConfig{
		Privileged: d.options.Privileged, CgroupnsMode: container.CgroupnsMode(d.options.CgroupNamespace),
		Tmpfs: d.options.Tmpfs, Binds: d.options.Binds,
	}, d.attachment(netip.Addr{}, node.Hostname), nil, d.nodeName(node.ID))
	if err != nil {
		return Instance{}, fmt.Errorf("create node: %w", err)
	}
	defer d.removeOnError(created.ID, &deployErr)

	data := node.UserData
	if len(data) == 0 {
		data = []byte("#cloud-config\n")
	}
	metadata := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", node.ID, node.Hostname)
	if err := d.copyFiles(ctx, created.ID, seedPath, map[string]string{
		"user-data": string(data), "meta-data": metadata,
	}); err != nil {
		return Instance{}, fmt.Errorf("copy cloud-init seed: %w", err)
	}
	return d.start(ctx, created.ID)
}

func (d *DockerDriver) ReserveIP(ctx context.Context, lease IPLease) (_ netip.Addr, leaseErr error) {
	if err := d.ensureImage(ctx, leaseImage); err != nil {
		return netip.Addr{}, err
	}
	created, err := d.client.ContainerCreate(ctx, &container.Config{
		Image: leaseImage, Labels: d.labels(), Cmd: []string{"tail", "-f", "/dev/null"},
	}, nil, d.attachment(lease.IP, lease.Hostname), nil, d.leaseName(lease.ID))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("reserve docker IP: %w", err)
	}
	defer d.removeOnError(created.ID, &leaseErr)
	instance, err := d.start(ctx, created.ID)
	return instance.IP, err
}

func (d *DockerDriver) ReleaseIP(ctx context.Context, id string) error {
	return d.remove(ctx, d.leaseName(id))
}

func (d *DockerDriver) leaseName(id string) string { return d.prefix + "-lease-" + id }

func (d *DockerDriver) removeOnError(id string, opErr *error) {
	if *opErr != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		*opErr = errors.Join(*opErr, d.remove(ctx, id))
	}
}

func (d *DockerDriver) start(ctx context.Context, id string) (Instance, error) {
	if err := d.client.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return Instance{}, fmt.Errorf("start container: %w", err)
	}
	details, err := d.client.ContainerInspect(ctx, id)
	if err != nil {
		return Instance{}, err
	}
	return d.instance(details)
}

func (d *DockerDriver) attachment(ip netip.Addr, alias string) *network.NetworkingConfig {
	endpoint := &network.EndpointSettings{NetworkID: d.networkID}
	if ip.IsValid() {
		endpoint.IPAMConfig = &network.EndpointIPAMConfig{IPv4Address: ip.String()}
	}
	if alias != "" {
		endpoint.Aliases = []string{alias}
	}
	return &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
		d.networkName: endpoint,
	}}
}

func (d *DockerDriver) nodeName(id string) string { return d.prefix + "-node-" + id }

func (d *DockerDriver) instance(details container.InspectResponse) (Instance, error) {
	if details.State == nil || !details.State.Running {
		return Instance{}, errors.New("container is not running")
	}
	if details.NetworkSettings == nil {
		return Instance{}, errors.New("container has no network settings")
	}
	endpoint := details.NetworkSettings.Networks[d.networkName]
	if endpoint == nil {
		return Instance{}, errors.New("container is not attached to the simulator network")
	}
	ip, err := netip.ParseAddr(endpoint.IPAddress)
	if err != nil {
		return Instance{}, fmt.Errorf("parse container IPv4: %w", err)
	}
	return Instance{IP: ip, MAC: endpoint.MacAddress}, nil
}

func (d *DockerDriver) Power(ctx context.Context, id string, on bool) (Instance, error) {
	name := d.nodeName(id)
	if !on {
		seconds := 10
		return Instance{}, d.client.ContainerStop(ctx, name, container.StopOptions{Timeout: &seconds})
	}
	return d.start(ctx, name)
}

func (d *DockerDriver) Release(ctx context.Context, id string) error {
	return d.remove(ctx, d.nodeName(id))
}

func (d *DockerDriver) remove(ctx context.Context, id string) error {
	err := d.client.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (d *DockerDriver) Cleanup(ctx context.Context) error {
	items, err := d.client.ContainerList(ctx, container.ListOptions{
		All: true, Filters: filters.NewArgs(filters.Arg("label", ownerLabel+"="+d.prefix)),
	})
	if err != nil {
		return err
	}
	for _, item := range items {
		err = errors.Join(err, d.remove(ctx, item.ID))
	}
	if d.ownsNetwork {
		removeErr := d.client.NetworkRemove(ctx, d.networkID)
		if !errdefs.IsNotFound(removeErr) {
			err = errors.Join(err, removeErr)
		}
	}
	return err
}

func (d *DockerDriver) copyFiles(ctx context.Context, id, dir string, files map[string]string) error {
	tarball, err := archiveFiles(files)
	if err != nil {
		return err
	}
	return d.client.CopyToContainer(ctx, id, dir, tarball, container.CopyToContainerOptions{})
}

func archiveFiles(files map[string]string) (*bytes.Reader, error) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for name, contents := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(contents))}); err != nil {
			return nil, err
		}
		if _, err := io.WriteString(writer, contents); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return bytes.NewReader(buffer.Bytes()), nil
}
