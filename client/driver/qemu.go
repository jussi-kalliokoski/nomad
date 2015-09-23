package driver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/nomad/structs"
)

var (
	reQemuVersion = regexp.MustCompile("QEMU emulator version ([\\d\\.]+).+")
)

// QemuDriver is a driver for running images via Qemu
// We attempt to chose sane defaults for now, with more configuration available
// planned in the future
type QemuDriver struct {
	DriverContext
}

// qemuHandle is returned from Start/Open as a handle to the PID
type qemuHandle struct {
	proc   *os.Process
	vmID   string
	waitCh chan error
	doneCh chan struct{}
}

// qemuPID is a struct to map the pid running the process to the vm image on
// disk
type qemuPID struct {
	Pid  int
	VmID string
}

// NewQemuDriver is used to create a new exec driver
func NewQemuDriver(ctx *DriverContext) Driver {
	return &QemuDriver{*ctx}
}

func (d *QemuDriver) Fingerprint(cfg *config.Config, node *structs.Node) (bool, error) {
	// Only enable if we are root when running on non-windows systems.
	if runtime.GOOS != "windows" && syscall.Geteuid() != 0 {
		d.logger.Printf("[DEBUG] driver.qemu: must run as root user, disabling")
		return false, nil
	}

	outBytes, err := exec.Command("qemu-system-x86_64", "-version").Output()
	if err != nil {
		return false, nil
	}
	out := strings.TrimSpace(string(outBytes))

	matches := reQemuVersion.FindStringSubmatch(out)
	if len(matches) != 2 {
		return false, fmt.Errorf("Unable to parse Qemu version string: %#v", matches)
	}

	node.Attributes["driver.qemu"] = "true"
	node.Attributes["driver.qemu.version"] = matches[1]

	return true, nil
}

// Run an existing Qemu image. Start() will pull down an existing, valid Qemu
// image and save it to the Drivers Allocation Dir
func (d *QemuDriver) Start(ctx *ExecContext, task *structs.Task) (DriverHandle, error) {
	// Get the image source
	source, ok := task.Config["image_source"]
	if !ok || source == "" {
		return nil, fmt.Errorf("Missing source image Qemu driver")
	}

	// Qemu defaults to 128M of RAM for a given VM. Instead, we force users to
	// supply a memory size in the tasks resources
	if task.Resources == nil || task.Resources.MemoryMB == 0 {
		return nil, fmt.Errorf("Missing required Task Resource: Memory")
	}

	// Attempt to download the thing
	// Should be extracted to some kind of Http Fetcher
	// Right now, assume publicly accessible HTTP url
	resp, err := http.Get(source)
	if err != nil {
		return nil, fmt.Errorf("Error downloading source for Qemu driver: %s", err)
	}

	// Create a location in the AllocDir to download and store the image.
	// TODO: Caching
	vmID := fmt.Sprintf("qemu-vm-%s-%s", structs.GenerateUUID(), filepath.Base(source))
	fPath := filepath.Join(ctx.AllocDir, vmID)
	vmPath, err := os.OpenFile(fPath, os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		return nil, fmt.Errorf("Error opening file to download to: %s", err)
	}

	defer vmPath.Close()
	defer resp.Body.Close()

	// Copy remote file to local AllocDir for execution
	// TODO: a retry of sort if io.Copy fails, for large binaries
	_, ioErr := io.Copy(vmPath, resp.Body)
	if ioErr != nil {
		return nil, fmt.Errorf("Error copying Qemu image from source: %s", ioErr)
	}

	// compute and check checksum
	if check, ok := task.Config["checksum"]; ok {
		d.logger.Printf("[DEBUG] Running checksum on (%s)", vmID)
		hasher := sha256.New()
		file, err := os.Open(vmPath.Name())
		if err != nil {
			return nil, fmt.Errorf("Failed to open file for checksum")
		}

		defer file.Close()
		io.Copy(hasher, file)

		sum := hex.EncodeToString(hasher.Sum(nil))
		if sum != check {
			return nil, fmt.Errorf(
				"Error in Qemu: checksums did not match.\nExpected (%s), got (%s)",
				check,
				sum)
		}
	}

	// Parse configuration arguments
	// Create the base arguments
	accelerator := "tcg"
	if acc, ok := task.Config["accelerator"]; ok {
		accelerator = acc
	}
	// TODO: Check a lower bounds, e.g. the default 128 of Qemu
	mem := fmt.Sprintf("%dM", task.Resources.MemoryMB)

	args := []string{
		"qemu-system-x86_64",
		"-machine", "type=pc,accel=" + accelerator,
		"-name", vmID,
		"-m", mem,
		"-drive", "file=" + vmPath.Name(),
		"-nodefconfig",
		"-nodefaults",
		"-nographic",
	}

	// TODO: Consolidate these into map of host/guest port when we have HCL
	// Note: Host port must be open and available
	if task.Config["guest_port"] != "" && task.Config["host_port"] != "" {
		args = append(args,
			"-netdev",
			fmt.Sprintf("user,id=user.0,hostfwd=tcp::%s-:%s",
				task.Config["host_port"],
				task.Config["guest_port"]),
			"-device", "virtio-net,netdev=user.0",
		)
	}

	// If using KVM, add optimization args
	if accelerator == "kvm" {
		args = append(args,
			"-enable-kvm",
			"-cpu", "host",
			// Do we have cores information available to the Driver?
			// "-smp", fmt.Sprintf("%d", cores),
		)
	}

	// Start Qemu
	var outBuf, errBuf bytes.Buffer
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	d.logger.Printf("[DEBUG] Starting QemuVM command: %q", strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf(
			"Error running QEMU: %s\n\nOutput: %s\n\nError: %s",
			err, outBuf.String(), errBuf.String())
	}

	d.logger.Printf("[INFO] Started new QemuVM: %s", vmID)

	// Create and Return Handle
	h := &qemuHandle{
		proc:   cmd.Process,
		vmID:   vmPath.Name(),
		doneCh: make(chan struct{}),
		waitCh: make(chan error, 1),
	}

	go h.run()
	return h, nil
}

func (d *QemuDriver) Open(ctx *ExecContext, handleID string) (DriverHandle, error) {
	// Parse the handle
	pidBytes := []byte(strings.TrimPrefix(handleID, "QEMU:"))
	qpid := &qemuPID{}
	if err := json.Unmarshal(pidBytes, qpid); err != nil {
		return nil, fmt.Errorf("failed to parse Qemu handle '%s': %v", handleID, err)
	}

	// Find the process
	proc, err := os.FindProcess(qpid.Pid)
	if proc == nil || err != nil {
		return nil, fmt.Errorf("failed to find Qemu PID %d: %v", qpid.Pid, err)
	}

	// Return a driver handle
	h := &qemuHandle{
		proc:   proc,
		vmID:   qpid.VmID,
		doneCh: make(chan struct{}),
		waitCh: make(chan error, 1),
	}

	go h.run()
	return h, nil
}

func (h *qemuHandle) ID() string {
	// Return a handle to the PID
	pid := &qemuPID{
		Pid:  h.proc.Pid,
		VmID: h.vmID,
	}
	data, err := json.Marshal(pid)
	if err != nil {
		log.Printf("[ERR] failed to marshal Qemu PID to JSON: %s", err)
	}
	return fmt.Sprintf("QEMU:%s", string(data))
}

func (h *qemuHandle) WaitCh() chan error {
	return h.waitCh
}

func (h *qemuHandle) Update(task *structs.Task) error {
	// Update is not possible
	return nil
}

// Kill is used to terminate the task. We send an Interrupt
// and then provide a 5 second grace period before doing a Kill.
//
// TODO: allow a 'shutdown_command' that can be executed over a ssh connection
// to the VM
func (h *qemuHandle) Kill() error {
	h.proc.Signal(os.Interrupt)
	select {
	case <-h.doneCh:
		return nil
	case <-time.After(5 * time.Second):
		return h.proc.Kill()
	}
}

func (h *qemuHandle) run() {
	ps, err := h.proc.Wait()
	close(h.doneCh)
	if err != nil {
		h.waitCh <- err
	} else if !ps.Success() {
		h.waitCh <- fmt.Errorf("task exited with error")
	}
	close(h.waitCh)
}
