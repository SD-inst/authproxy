package watchdog

import (
	"log"
	"os"
)

type Watchdog struct {
	fifoPath string
}

func NewWatchdog(fifoPath string) *Watchdog {
	return &Watchdog{fifoPath: fifoPath}
}

func (wd *Watchdog) Send(s string) {
	go func() {
		f, err := os.OpenFile(wd.fifoPath, os.O_WRONLY, 0666)
		if err != nil {
			log.Printf("Error opening control FIFO: %s", err)
			return
		}
		f.WriteString(s)
		f.Close()
		log.Printf("Command %s sent", s)
	}()
}
