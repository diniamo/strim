package mpv

import (
	"os"
	"os/exec"
	"time"

	log "github.com/diniamo/glog"
	"github.com/diniamo/gopv"
)

const ipcDelay = 10 * time.Millisecond
const ipcAttempts = 100

func Open(args ...string) (*exec.Cmd, *gopv.Client, error) {
	ipcPath, err := gopv.GeneratePath()
	if err != nil {
		return nil, nil, err
	}

	args = append(
		args,
		"--input-ipc-server=" + ipcPath,
		"--quiet",
		// Imprecise seeks may result in multiple seconds of desync,
		// since what would be the precise seek is reported anyway
		"--hr-seek=yes",
	)
	
	cmd := exec.Command("mpv", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err = cmd.Start()
	if err != nil {
		return nil, nil, err
	}

	for range ipcAttempts {
		ipcClient, err := gopv.Connect(ipcPath, func(err error) {
			log.Errorf("IPC error: %s", err)
		})

		if err == nil {
			return cmd, ipcClient, nil
		}
		
		time.Sleep(ipcDelay)
	}
		
	return nil, nil, err
}
