package driver

import (
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strings"

	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/nomad/structs"
)

var (
	reDockerVersion = regexp.MustCompile("Docker version (\\d\\.+),.+")
	reDockerSha     = regexp.MustCompile("^[a-f0-9]{64}$")
)

type DockerDriver struct {
	logger *log.Logger
}

type dockerPID struct {
	imageID     string
	containerID string
}

type dockerHandle struct {
	logger      *log.Logger
	imageID     string
	containerID string
	waitCh      chan error
	doneCh      chan struct{}
}

func NewDockerDriver(logger *log.Logger) Driver {
	d := &DockerDriver{
		logger: logger,
	}
	return d
}

func (d *DockerDriver) Fingerprint(cfg *config.Config, node *structs.Node) (bool, error) {
	outBytes, err := exec.Command("docker", "-v").Output()
	out := string(outBytes)
	if err != nil {
		return false, nil
	}

	matches := reDockerVersion.FindStringSubmatch(out)
	if len(matches) != 2 {
		return false, fmt.Errorf("Unable to parse docker version string")
	}

	node.Attributes["driver.docker"] = "true"
	node.Attributes["driver.docker.version"] = matches[1]

	return true, nil
}

func (d *DockerDriver) Start(ctx *ExecContext, task *structs.Task) (DriverHandle, error) {
	// Get the image from config
	image, ok := task.Config["image"]
	if !ok || image == "" {
		return nil, fmt.Errorf("Image not specified")
	}

	// Download the image
	pull, err := exec.Command("docker", "pull", image).CombinedOutput()
	if err != nil {
		d.logger.Printf("[ERROR] driver.docker %s", pull)
		return nil, fmt.Errorf("Failed to pull `%s`: %s", image, err)
	}
	d.logger.Printf("[DEBUG] docker pull %s:\n%s", image, pull)

	// Get the image ID (sha256). We need to keep track of this in case another
	// process pulls down a newer version of the image.
	imageIDBytes, err := exec.Command("docker", "images", "-q", "--no-trunc", image).CombinedOutput()
	imageID := strings.TrimSpace(string(imageIDBytes))
	if err != nil || imageID == "" {
		d.logger.Printf("[ERROR] driver.docker %s", imageID)
		return nil, fmt.Errorf("Failed to determine image id for `%s`: %s", image, err)
	}
	if !reDockerSha.MatchString(imageID) {
		return nil, fmt.Errorf("Image id not in expected format (sha256); found %s", imageID)
	}
	d.logger.Println("[DEBUG] driver.docker using image %s", imageID)

	// Create a container
	containerIDBytes, err := exec.Command("docker", "create", imageID).CombinedOutput()
	containerID := strings.TrimSpace(string(containerIDBytes))
	if err != nil {
		d.logger.Printf("[ERROR] driver.docker %s", containerID)
		return nil, fmt.Errorf("Failed to create container from image %s", image)
	}
	if !reDockerSha.MatchString(containerID) {
		return nil, fmt.Errorf("Container id not in expected format (sha256); found %s", containerID)
	}
	d.logger.Println("[DEBUG] driver.docker created container %s", containerID)

	// Start the container. The output is containerID again so don't need to
	// validate it. Also, the docker daemon is responsible for running this so
	// start will return immediately. We'll use the containerID with the docker
	// watch command to track its status.
	//
	// Note: at some point we will want to use docker run instead, to set extra
	// options and such. You should use docker run -d and still use wait to
	// check whether the process is available so we are able to re-wait if the
	// nomad process is restarted. Also, you will need to parse the containerID
	// out of the run command output since run combines pull, create and start
	// into a single command.
	startBytes, err := exec.Command("docker", "start", containerID).CombinedOutput()
	start := strings.TrimSpace(string(startBytes))
	if err != nil {
		d.logger.Printf("[ERROR] driver.docker %s", start)
		return nil, fmt.Errorf("Failed to start container %s", containerID)
	}

	// Return a driver handle
	h := &dockerHandle{
		imageID:     imageID,
		containerID: containerID,
		doneCh:      make(chan struct{}),
		waitCh:      make(chan error, 1),
	}
	return h, nil
}

func (d *DockerDriver) Open(ctx *ExecContext, handleID string) (DriverHandle, error) {
	// Split the handle
	pidStr := strings.TrimPrefix(handleID, "DOCKER:")
	pid := &dockerPID{}
	err := json.Unmarshal([]byte(pidStr), pid)
	if err != nil {
		return nil, fmt.Errorf("Failed to parse handle '%s': %v", handleID, err)
	}

	// Look for a running container with this ID
	// docker ps does not return an exit code if there are no matching processes
	// so we have to read the output and compare it to our known containerID
	psBytes, err := exec.Command("docker", "ps", "-q", "--no-trunc",
		fmt.Sprintf("-f=id=%s", pid.containerID)).Output()
	ps := strings.TrimSpace(string(psBytes))
	if err != nil || ps != pid.containerID {
		return nil, fmt.Errorf("Failed to find container %s: %v", pid.containerID, err)
	}

	// Return a driver handle
	h := &dockerHandle{
		imageID:     pid.imageID,
		containerID: pid.containerID,
		doneCh:      make(chan struct{}),
		waitCh:      make(chan error, 1),
	}
	go h.run()
	return h, nil
}

func (h *dockerHandle) ID() string {
	// Return a handle to the PID
	pid := dockerPID{
		imageID:     h.imageID,
		containerID: h.containerID,
	}
	data, err := json.Marshal(pid)
	if err != nil {
		log.Printf("[ERROR] failed to marshal docker PID to JSON: %s", err)
	}
	return fmt.Sprintf("DOCKER:%s", string(data))
}

func (h *dockerHandle) WaitCh() chan error {
	return h.waitCh
}

func (h *dockerHandle) Update(task *structs.Task) error {
	// Update is not possible
	return nil
}

// Kill is used to terminate the task. We send an Interrupt
// and then provide a 5 second grace period before doing a Kill.
func (h *dockerHandle) Kill() error {
	// Stop the container
	stop, err := exec.Command("docker", "stop", "-t", "5", h.containerID).CombinedOutput()
	if err != nil {
		log.Printf("[ERROR] driver.docker %s", stop)
		return fmt.Errorf("Failed to stop container %s: %s", h.containerID, err)
	}

	// Cleanup container
	rmContainer, err := exec.Command("docker", "rm", h.containerID).CombinedOutput()
	if err != nil {
		log.Printf("[ERROR] driver.docker %s", rmContainer)
		return fmt.Errorf("Failed to remove container %s: %s", h.containerID, err)
	}

	// Cleanup image. This operation may fail if the image is in use by another
	// job. That is OK. Will we log a message but continue.
	_, err = exec.Command("docker", "rmi", h.imageID).CombinedOutput()
	if err != nil {
		log.Printf("[WARN] driver.docker failed to remove image %s; it may still be in use", h.imageID)
	}
	return nil
}

func (h *dockerHandle) run() {
	// Wait for it...
	waitBytes, err := exec.Command("docker", "wait", h.containerID).Output()
	if err != nil {
		h.logger.Printf("[ERROR] driver.docker unable to wait for %s; container already terminated", h.containerID)
	}
	wait := strings.TrimSpace(string(waitBytes))

	// If the container failed, try to get the last 10 lines of logs for our
	// error message.
	if wait != "0" {
		var logsBytes []byte
		logsBytes, err := exec.Command("docker", "logs", "--tail=10", h.containerID).Output()
		logs := string(logsBytes)
		if err == nil {
			err = fmt.Errorf("%s", logs)
		}
	}

	close(h.doneCh)
	if err != nil {
		h.waitCh <- err
	}
	close(h.waitCh)
}
