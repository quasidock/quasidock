package run

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/quasidock/quasidock/internal/environment"
	"github.com/quasidock/quasidock/internal/process"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

// TestHCNJobAttachment probes the native runtime before exposing it in the CLI.
// Run with QUASIDOCK_TEST_HCN=1 in an elevated PowerShell session.
func TestHCNJobAttachment(t *testing.T) {
	if os.Getenv("QUASIDOCK_TEST_HCN") != "1" {
		t.Skip("requires an elevated Windows host with HNS and private virtual switch support")
	}
	token, err := win32.OpenProcessToken(win32.GetCurrentProcess(), win32.TOKEN_QUERY|win32.TOKEN_DUPLICATE|win32.TOKEN_ADJUST_DEFAULT|win32.TOKEN_ASSIGN_PRIMARY)
	if err != nil {
		t.Fatal(err)
	}
	defer win32.CloseHandle(token)
	rid, err := win32.GetTokenIntegrityRid(token)
	if err != nil {
		t.Fatal(err)
	}
	if rid < 0x3000 {
		t.Fatal("run this test from an elevated PowerShell session")
	}
	medium := createHCNProbePeerToken(t, token)
	defer win32.CloseHandle(medium)
	if rid, err := win32.GetTokenIntegrityRid(medium); err != nil || rid >= 0x3000 {
		t.Fatalf("expected an unelevated payload token, got integrity %#x: %v", rid, err)
	}

	hnsdiag, err := win32.SystemProgram("hnsdiag.exe")
	if err != nil {
		t.Fatal(err)
	}
	owner := "quasidock-probe-" + rand.Text()
	network, err := win32.CreatePrivateHCNNetwork(owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if out, err := exec.Command(hnsdiag, "delete", "networks", network).CombinedOutput(); err != nil {
			t.Errorf("delete test network %s: %v\n%s", network, err, out)
		}
	})

	prepare := func() (*networkDriver, win32.HANDLE, store.NetworkAttachment) {
		t.Helper()
		driver := &networkDriver{owner: owner, parent: win32.GetCurrentProcess()}
		job, err := process.NewJob()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { win32.CloseHandle(job) })
		if err := driver.begin(job); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := driver.end(); err != nil {
				t.Error(err)
			}
		})
		reply, err := driver.connect(network)
		if err != nil {
			t.Fatal(err)
		}
		return driver, job, reply.Endpoint
	}
	_, serverJob, serverAttachment := prepare()
	client, clientJob, clientAttachment := prepare()
	if clientAttachment.IPAddress == serverAttachment.IPAddress {
		t.Fatal("the network allocated the same address to both jobs")
	}
	_, serverOutput := startHCNProbePeer(t, serverJob, medium, "listen")
	t.Log(scanHCNProbe(t, serverOutput))
	port := scanHCNProbe(t, serverOutput)
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		t.Fatalf("read server port: %q", port)
	}
	clientInput, clientOutput := startHCNProbePeer(t, clientJob, medium, net.JoinHostPort(serverAttachment.IPAddress, port))
	t.Log(scanHCNProbe(t, clientOutput))
	expect := func(want string) {
		t.Helper()
		if _, err := fmt.Fprintln(clientInput, "try"); err != nil {
			t.Fatal(err)
		}
		if got := scanHCNProbe(t, clientOutput); !strings.HasPrefix(got, want) {
			t.Fatalf("probe result = %q, want %q", got, want)
		}
	}
	expect("echo")
	if _, err := client.disconnect(network); err != nil {
		t.Fatal(err)
	}
	expect("unreachable")
	if err := client.restore(network); err != nil {
		t.Fatal(err)
	}
	expect("echo")
}

func createHCNProbePeerToken(t *testing.T, current win32.HANDLE) win32.HANDLE {
	t.Helper()
	path, err := win32.SystemProgram("advapi32.dll")
	if err != nil {
		t.Fatal(err)
	}
	create := syscall.NewLazyDLL(path).NewProc("CreateRestrictedToken")
	if err := create.Find(); err != nil {
		t.Fatal(err)
	}
	admin, err := win32.ConvertStringSidToSid("S-1-5-32-544")
	if err != nil {
		t.Fatal(err)
	}
	defer win32.LocalFree(uintptr(admin))
	denyOnly := win32.SID_AND_ATTRIBUTES{Sid: admin}
	const disableMaxPrivilege = 1
	var restricted win32.HANDLE
	ok, _, err := create.Call(current, disableMaxPrivilege, 1, uintptr(unsafe.Pointer(&denyOnly)), 0, 0, 0, 0, uintptr(unsafe.Pointer(&restricted)))
	if ok == 0 {
		t.Fatalf("create a restricted payload token: %v", err)
	}
	integrity, err := win32.ConvertStringSidToSid("S-1-16-8192")
	if err != nil {
		win32.CloseHandle(restricted)
		t.Fatal(err)
	}
	defer win32.LocalFree(uintptr(integrity))
	if err := win32.SetTokenIntegrityLevel(restricted, integrity); err != nil {
		win32.CloseHandle(restricted)
		t.Fatalf("set medium integrity on payload token: %v", err)
	}
	return restricted
}

func startHCNProbePeer(t *testing.T, job, token win32.HANDLE, role string) (*os.File, *bufio.Scanner) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env, err := environment.EncodeBlock([]environment.Var{
		{Name: "QUASIDOCK_TEST_HCN_PEER", Value: role},
		{Name: "SystemRoot", Value: os.Getenv("SystemRoot")},
	})
	if err != nil {
		t.Fatal(err)
	}
	stdin, in, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	t.Cleanup(func() { _ = in.Close() })
	out, stdout, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	t.Cleanup(func() { _ = out.Close() })
	stdio := [3]win32.HANDLE{stdin.Fd(), stdout.Fd(), stdout.Fd()}
	handle, err := process.StartIn(job, process.SpawnSpec{
		Exe: exe, CommandLine: process.BuildCommandLine(exe, []string{"-test.run=^TestHCNProbePeer$"}),
		Token: token, EnvBlock: env, Stdio: &stdio, PreserveConsoleHandler: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		win32.TerminateProcess(handle, 0)
		win32.WaitForSingleObject(handle, 5000)
		win32.CloseHandle(handle)
	})
	return in, bufio.NewScanner(out)
}

func scanHCNProbe(t *testing.T, scanner *bufio.Scanner) string {
	t.Helper()
	line := make(chan string, 1)
	go func() {
		if scanner.Scan() {
			line <- strings.TrimSpace(scanner.Text())
		}
		close(line)
	}()
	select {
	case value, ok := <-line:
		if !ok {
			t.Fatalf("probe process exited: %v", scanner.Err())
		}
		return value
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the probe process")
		return ""
	}
}

func TestHCNProbePeer(t *testing.T) {
	role := os.Getenv("QUASIDOCK_TEST_HCN_PEER")
	if role == "" {
		t.Skip("subprocess helper")
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	var addresses []string
	for _, iface := range ifaces {
		for _, address := range mustHCNProbeIfaceAddrs(t, iface) {
			addresses = append(addresses, iface.Name+"="+address.String())
		}
	}
	fmt.Println(os.Getpid(), addresses)
	if role == "listen" {
		listener, err := net.Listen("tcp4", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		fmt.Println(listener.Addr().(*net.TCPAddr).Port)
		for {
			conn, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(conn, "quasidock")
			_ = conn.Close()
		}
	}

	commands := bufio.NewScanner(os.Stdin)
	for commands.Scan() {
		result := "unreachable"
		conn, err := net.DialTimeout("tcp4", role, 5*time.Second)
		if err == nil {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			response, readErr := io.ReadAll(conn)
			_ = conn.Close()
			if string(response) == "quasidock" && readErr == nil {
				result = "echo"
			}
		} else {
			result += " " + err.Error()
		}
		fmt.Println(result)
	}
}

func mustHCNProbeIfaceAddrs(t *testing.T, iface net.Interface) []net.Addr {
	t.Helper()
	addresses, err := iface.Addrs()
	if err != nil {
		t.Fatal(err)
	}
	return addresses
}
