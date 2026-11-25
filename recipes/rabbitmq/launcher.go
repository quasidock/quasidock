// Native Windows launchers for RabbitMQ. Broker children remain in Quasidock's
// job; control tools read this data directory's current EPMD endpoint.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const version = "4.4.1"

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}
type endpoint struct {
	Node         string `json:"node"`
	EPMD         int    `json:"epmd_port"`
	Distribution int    `json:"distribution_port"`
}

func secret() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func create(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func readJSON(path string, value any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, value)
}

func env(name, value string) {
	if err := os.Setenv(name, value); err != nil {
		panic(err)
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	p := l.Addr().(*net.TCPAddr).Port
	return p, l.Close()
}

func command(exe string, args ...string) *exec.Cmd {
	c := exec.Command(exe, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Only this supervisor handles console events. In particular EPMD must
	// survive Ctrl+Break long enough for rabbitmqctl to shut the broker down.
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	return c
}

func status(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	fmt.Fprintln(os.Stderr, "rabbitmq:", err)
	return 1
}

func run() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	root := filepath.Dir(filepath.Dir(self))
	name := strings.TrimSuffix(filepath.Base(self), ".exe")
	args := os.Args[1:]
	server := name == "rabbitmq-server"
	if server && len(args) > 0 {
		switch args[0] {
		case "--version", "version":
			fmt.Println("RabbitMQ " + version)
			return nil
		case "--help", "-h":
			fmt.Println("rabbitmq-server [Erlang VM arguments]\nStarts a foreground broker. Data: RABBITMQ_BASE. Ports: --port=amqp=5672 --port=management=15672 on quasidock run.")
			return nil
		}
	}
	base := os.Getenv("RABBITMQ_BASE")
	if base == "" {
		return errors.New("RABBITMQ_BASE must name the container data directory")
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return err
	}
	erlang := os.Getenv("ERLANG_HOME")
	if erlang == "" {
		return errors.New("ERLANG_HOME is unset")
	}
	erl := filepath.Join(erlang, "bin", "erl.exe")
	home := filepath.Join(base, "home")
	for key, value := range map[string]string{
		"RABBITMQ_HOME": root, "HOME": home, "USERPROFILE": home,
		"RABBITMQ_MNESIA_DIR":           filepath.Join(base, "mnesia"),
		"RABBITMQ_LOG_BASE":             filepath.Join(base, "log"),
		"RABBITMQ_CONFIG_FILE":          filepath.Join(base, "rabbitmq.conf"),
		"RABBITMQ_ENABLED_PLUGINS_FILE": filepath.Join(base, "enabled_plugins"),
		"RABBITMQ_PLUGINS_DIR":          filepath.Join(root, "plugins"),
		"RABBITMQ_PLUGINS_EXPAND_DIR":   filepath.Join(base, "plugins-expand"),
		"ERL_CRASH_DUMP":                filepath.Join(base, "erl_crash.dump"),
		"ERL_EPMD_ADDRESS":              "127.0.0.1",
	} {
		env(key, value)
	}
	var ep endpoint
	state := filepath.Join(base, "runtime.json")
	if !server {
		if err := readJSON(state, &ep); err == nil {
			if ep.EPMD < 1 || ep.EPMD > 65535 || ep.Node == "" {
				return errors.New("invalid broker runtime.json")
			}
			env("RABBITMQ_NODENAME", ep.Node)
			env("ERL_EPMD_PORT", strconv.Itoa(ep.EPMD))
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("reading broker endpoint: %w", err)
		}
		cliArgs := []string{"+B", "-boot", "start_clean", "-noinput", "-noshell", "-hidden", "-start_epmd", "false", "-kernel", "inet_dist_listen_min", "0", "inet_dist_listen_max", "0", "-run", "escript", "start", "-escript", "main", "Elixir.RabbitMQCtl", "-extra", filepath.Join(root, "escript", name)}
		return command(erl, append(cliArgs, args...)...).Run()
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		return err
	}
	lockPath, err := syscall.UTF16PtrFromString(filepath.Join(base, ".rabbitmq.lock"))
	if err != nil {
		return err
	}
	lock, err := syscall.CreateFile(lockPath, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return fmt.Errorf("data directory is already in use or cannot be locked: %w", err)
	}
	defer syscall.CloseHandle(lock)
	credPath := filepath.Join(base, "credentials.json")
	var cred credentials
	if err := readJSON(credPath, &cred); errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Stat(filepath.Join(base, "mnesia")); statErr == nil {
			return errors.New("broker data exists without credentials.json; refusing to reset credentials")
		}
		cred = credentials{Username: "admin", Password: secret()}
		b, _ := json.MarshalIndent(cred, "", "  ")
		if err := create(credPath, append(b, '\n')); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if cred.Username == "" || cred.Password == "" {
		return errors.New("credentials.json has an empty username or password")
	}
	cookie := filepath.Join(home, ".erlang.cookie")
	nodePath := filepath.Join(base, "node.txt")
	for _, path := range []string{cookie, nodePath} {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			if _, dataErr := os.Stat(filepath.Join(base, "mnesia")); dataErr == nil {
				return fmt.Errorf("broker data exists without %s; refusing to replace identity", path)
			}
		} else if err != nil {
			return err
		}
	}
	if err := create(cookie, []byte(secret())); err != nil {
		return err
	}
	if err := create(nodePath, []byte("rabbit_"+secret()[:16]+"@localhost")); err != nil {
		return err
	}
	node, err := os.ReadFile(nodePath)
	if err != nil {
		return err
	}
	ep.Node = strings.TrimSpace(string(node))
	if !strings.HasPrefix(ep.Node, "rabbit_") || !strings.HasSuffix(ep.Node, "@localhost") {
		return errors.New("invalid persistent node identity")
	}
	if err := create(filepath.Join(base, "rabbitmq.conf"), []byte("listeners.tcp.1 = 127.0.0.1:$(RABBITMQ_NODE_PORT)\nmanagement.tcp.ip = 127.0.0.1\nmanagement.tcp.port = $(QUASIDOCK_RABBITMQ_MANAGEMENT_PORT)\ndistribution.listener.interface = 127.0.0.1\nlog.console = true\nlog.file = false\ndefault_user_tags.administrator = true\n")); err != nil {
		return err
	}
	if err := create(filepath.Join(base, "enabled_plugins"), []byte("[rabbitmq_management].\n")); err != nil {
		return err
	}
	env("RABBITMQ_DEFAULT_USER", cred.Username)
	env("RABBITMQ_DEFAULT_PASS", cred.Password)
	env("RABBITMQ_NODENAME", ep.Node)
	// The EPMD process has a dedicated lifetime and endpoint, never the host's 4369.
	var epmd *exec.Cmd
	var epmdDone chan error
	for attempt := 0; attempt < 10; attempt++ {
		ep.EPMD, err = freePort()
		if err != nil {
			return err
		}
		env("ERL_EPMD_PORT", strconv.Itoa(ep.EPMD))
		epmd = command(filepath.Join(erlang, "erts-17.1.2", "bin", "epmd.exe"), "-address", "127.0.0.1", "-port", strconv.Itoa(ep.EPMD))
		if err = epmd.Start(); err != nil {
			return err
		}
		epmdDone = make(chan error, 1)
		go func(c *exec.Cmd, done chan error) { done <- c.Wait() }(epmd, epmdDone)
		ready := false
		for i := 0; i < 40; i++ {
			select {
			case <-epmdDone:
				i = 40
				continue
			default:
			}
			conn, dialErr := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", ep.EPMD), 100*time.Millisecond)
			if dialErr == nil {
				conn.Close()
				ready = true
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if ready {
			break
		}
		_ = epmd.Process.Kill()
		epmd = nil
	}
	if epmd == nil {
		return errors.New("could not start private EPMD")
	}
	defer epmd.Process.Kill()
	ep.Distribution, err = freePort()
	if err != nil {
		return err
	}
	env("RABBITMQ_DIST_PORT", strconv.Itoa(ep.Distribution))
	b, _ := json.MarshalIndent(ep, "", "  ")
	if err := os.WriteFile(state, append(b, '\n'), 0600); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "RabbitMQ credentials:", credPath)
	env("ERL_LIBS", filepath.Join(root, "plugins"))
	brokerArgs := []string{"+Bi", "-noinput", "-boot", "start_sasl", "-start_epmd", "false", "-s", "rabbit", "boot", "+W", "w", "+MBas", "ageffcbf", "+MHas", "ageffcbf", "+MBlmbcs", "512", "+MHlmbcs", "512", "+MMmcs", "30", "+pc", "unicode", "-syslog", "logger", "[]", "-syslog", "syslog_error_logger", "false", "-kernel", "prevent_overlapping_partitions", "false"}
	broker := command(erl, append(brokerArgs, args...)...)
	interrupt := make(chan os.Signal, 2)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupt)
	if err := broker.Start(); err != nil {
		return err
	}
	defer broker.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- broker.Wait() }()
	select {
	case err := <-done:
		return err
	case err := <-epmdDone:
		return fmt.Errorf("private EPMD exited while broker was running: %v", err)
	case <-interrupt:
		shutdown := command(filepath.Join(root, "bin", "rabbitmqctl.exe"), "shutdown")
		if err := shutdown.Start(); err != nil {
			return err
		}
		defer shutdown.Process.Kill()
		go shutdown.Wait()
		return <-done
	}
}

func main() { os.Exit(status(run())) }
