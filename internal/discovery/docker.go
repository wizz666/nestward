package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Container is the part of Docker's /containers/json answer we use.
type Container struct {
	ID         string            `json:"Id"`
	Names      []string          `json:"Names"`
	Image      string            `json:"Image"`
	State      string            `json:"State"`
	Status     string            `json:"Status"`
	Ports      []ContainerPort   `json:"Ports"`
	Labels     map[string]string `json:"Labels"`
	HostConfig struct {
		NetworkMode string `json:"NetworkMode"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// ContainerPort is one published or exposed port.
type ContainerPort struct {
	IP          string `json:"IP"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort"`
	Type        string `json:"Type"`
}

// ErrNoDocker means no Docker socket was found.
var ErrNoDocker = errors.New("docker not found")

// DockerClient talks to the Docker Engine API. It only ever sends GET
// requests: Nestward looks at containers, it never changes them.
type DockerClient struct {
	http *http.Client
	base string
}

// NewDockerClient connects to DOCKER_HOST (unix:// or tcp://, e.g. a
// read-only socket proxy) or /var/run/docker.sock.
func NewDockerClient() (*DockerClient, error) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("DOCKER_HOST: %w", err)
	}
	tr := &http.Transport{MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second}
	c := &DockerClient{http: &http.Client{Transport: tr, Timeout: 15 * time.Second}}
	switch u.Scheme {
	case "unix":
		sock := u.Path
		if _, err := os.Stat(sock); err != nil {
			return nil, ErrNoDocker
		}
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}
		c.base = "http://docker"
	case "tcp", "http":
		c.base = "http://" + u.Host
	default:
		return nil, fmt.Errorf("DOCKER_HOST scheme %q is not supported", u.Scheme)
	}
	return c, nil
}

// Containers lists all containers, running or stopped.
func (c *DockerClient) Containers(ctx context.Context) ([]Container, error) {
	var out []Container
	err := c.get(ctx, "/containers/json?all=1", &out)
	return out, err
}

func (c *DockerClient) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("docker %s: %s %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(v)
}

// FileLister reads a saved /containers/json answer, for tests and for
// looking at another machine's list without access to its Docker.
type FileLister string

// Containers reads the file.
func (f FileLister) Containers(context.Context) ([]Container, error) {
	b, err := os.ReadFile(string(f))
	if err != nil {
		return nil, err
	}
	var cs []Container
	if err := json.Unmarshal(b, &cs); err != nil {
		return nil, fmt.Errorf("%s: %w", f, err)
	}
	return cs, nil
}
