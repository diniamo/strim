package mpv

import (
	"os"
	"os/exec"
	"runtime"
	"time"

	log "github.com/diniamo/glog"
	"github.com/diniamo/gopv"
)

func Open(args ...string) (*exec.Cmd, *gopv.Client, error) {
	ipcPath, err := gopv.GeneratePath()
	if err != nil {
		return nil, nil, err
	}

	path := "mpv"
	if runtime.GOOS == "windows" {
		path = "mpv.exe"
	}
	args = append(
		args,
		"--input-ipc-server=" + ipcPath,
		"--quiet",
		// Imprecise seeks may result in multiple seconds of desync,
		// since what would be the precise seek is reported anyway
		"--hr-seek=yes",
	)
	
	cmd := exec.Command(path, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err = cmd.Start()
	if err != nil {
		return nil, nil, err
	}

	time.Sleep(2 * time.Second)
	
	ipcClient, err := gopv.Connect(ipcPath, func(err error) {
		log.Errorf("Mpv IPC error: %s", err)
	})
	if err != nil {
		cmd.Cancel()
		return nil, nil, err
	}

	return cmd, ipcClient, nil
}
