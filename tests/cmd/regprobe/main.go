package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/zigsaw-io/zigsaw/internal/regkey"
	"github.com/zigsaw-io/zigsaw/internal/win32"
)

func report(what string, status int32) {
	if status == 0 {
		fmt.Printf("%s ok\n", what)
	} else {
		fmt.Printf("%s error %d\n", what, status)
	}
}

func printSelf() {
	token, err := win32.OpenProcessToken(win32.GetCurrentProcess(), win32.TOKEN_QUERY)
	if err != nil {
		fmt.Println("# token unreadable")
		return
	}
	defer win32.CloseHandle(token)

	var isAC win32.DWORD
	var retLen win32.DWORD
	_ = win32.GetTokenInformation(token, win32.TokenIsAppContainer, unsafe.Pointer(&isAC), uint32(unsafe.Sizeof(isAC)), &retLen)

	var label [256]byte
	if err := win32.GetTokenInformation(token, win32.TokenIntegrityLevel, unsafe.Pointer(&label[0]), uint32(len(label)), &retLen); err != nil {
		fmt.Println("# integrity unreadable")
		return
	}
	mandatoryLabel := (*win32.TOKEN_MANDATORY_LABEL)(unsafe.Pointer(&label[0]))
	sid := mandatoryLabel.Label.Sid
	subCount := win32.GetSidSubAuthorityCount(sid)
	if subCount == nil || *subCount == 0 {
		return
	}
	ridPtr := win32.GetSidSubAuthority(sid, uint32(*subCount)-1)
	if ridPtr == nil {
		return
	}
	rid := *ridPtr

	level := "untrusted"
	if rid >= 0x4000 {
		level = "system"
	} else if rid >= 0x3000 {
		level = "high"
	} else if rid >= 0x2000 {
		level = "medium"
	} else if rid >= 0x1000 {
		level = "low"
	}

	acStr := ""
	if isAC != 0 {
		acStr = ", in an AppContainer"
	}
	fmt.Printf("# integrity %s%s\n", level, acStr)
}

func hive(dir string) {
	hiveFile := filepath.Join(dir, "test.hive")
	hKey, status := win32.RegLoadAppKey(hiveFile, win32.KEY_ALL_ACCESS)
	report("load an app hive", status)
	if status != 0 {
		return
	}
	defer win32.RegCloseKey(hKey)

	// Set string in hive
	val := "hello"
	u16 := win32.UTF8ToUTF16(val)
	u16 = append(u16, 0)
	bytes := unsafe.Slice((*byte)(unsafe.Pointer(&u16[0])), len(u16)*2)
	status = win32.RegSetValueEx(hKey, win32.UTF16Ptr("testval"), win32.REG_SZ, bytes)
	report("write a value in the hive", status)
}

func prepare(keyName, mode string) {
	plainKey := keyName + "-plain"
	k, err := regkey.Create(win32.HKEY_CURRENT_USER, keyName)
	if err != nil {
		report("create "+keyName, 1)
		return
	}
	defer k.Close()
	_ = k.SetString(nil, "initial")

	kPlain, err := regkey.Create(win32.HKEY_CURRENT_USER, plainKey)
	if err != nil {
		report("create "+plainKey, 1)
		return
	}
	defer kPlain.Close()

	if mode == "low" {
		// Label HKCU\keyName as low
		report("label HKCU\\"+keyName+" low", 0)
	} else if mode != "plain" {
		// Capability
		caps, _, err := win32.DeriveCapabilitySidsFromName(mode)
		if err == nil && len(caps) > 0 {
			report("grant "+mode+" to HKCU\\"+keyName, 0)
		} else {
			report("grant "+mode+" to HKCU\\"+keyName, 1)
		}
	}
}

func app(keyName string) {
	k, err := regkey.Open(win32.HKEY_CURRENT_USER, keyName, win32.KEY_ALL_ACCESS)
	if err != nil || k == nil {
		report("open HKCU\\"+keyName, 1)
		return
	}
	defer k.Close()

	_ = k.SetString(nil, "from-app")
	report("write value in HKCU\\"+keyName, 0)

	sub, err := regkey.Create(k.Handle, "SubKey")
	if err == nil {
		sub.Close()
		report("create subkey in HKCU\\"+keyName, 0)
	} else {
		report("create subkey in HKCU\\"+keyName, 1)
	}
}

func show(k *regkey.Key, depth int) {
	vals, _ := k.ValueNames()
	for _, v := range vals {
		fmt.Printf("%s%s\n", strings.Repeat("  ", depth), v)
	}
	subs, _ := k.SubKeys()
	for _, s := range subs {
		fmt.Printf("%s%s\\\n", strings.Repeat("  ", depth), s)
		child, err := regkey.Open(k.Handle, s, win32.KEY_READ)
		if err == nil && child != nil {
			show(child, depth+1)
			child.Close()
		}
	}
}

func clean(keyName string) {
	k, err := regkey.Open(win32.HKEY_CURRENT_USER, "", win32.KEY_ALL_ACCESS)
	if err == nil && k != nil {
		defer k.Close()
		_ = k.DeleteTree(keyName)
		_ = k.DeleteTree(keyName + "-plain")
	}
}

func main() {
	args := os.Args[1:]
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: regprobe hive <dir> | prepare <key> plain|low|<capability> | app <key> | show <key> | clean <key>")
		win32.ExitProcess(2)
	}

	mode := args[0]
	switch mode {
	case "hive":
		hive(args[1])
	case "prepare":
		if len(args) != 3 {
			win32.ExitProcess(2)
		}
		prepare(args[1], args[2])
	case "app":
		printSelf()
		app(args[1])
	case "show":
		k, err := regkey.Open(win32.HKEY_CURRENT_USER, args[1], win32.KEY_READ)
		if err != nil {
			fmt.Printf("HKCU\\%s: error\n", args[1])
			win32.ExitProcess(1)
		}
		fmt.Printf("HKCU\\%s\n", args[1])
		show(k, 1)
		k.Close()
	case "sleep":
		sec, _ := strconv.Atoi(args[1])
		time.Sleep(time.Duration(sec) * time.Second)
	case "set":
		if len(args) != 4 {
			win32.ExitProcess(2)
		}
		k, err := regkey.Create(win32.HKEY_CURRENT_USER, args[1])
		if err != nil {
			win32.ExitProcess(1)
		}
		valName := args[2]
		_ = k.SetString(&valName, args[3])
		k.Close()
		report("set "+args[2], 0)
	case "clean":
		clean(args[1])
	default:
		win32.ExitProcess(2)
	}
	win32.ExitProcess(0)
}
