package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/rkfg/authproxy/civitai"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("Provide the model path as the only argument")
	}
	dl := civitai.NewDownloader()
	filename := os.Args[1]
	err := dl.Walk(filename, func(path string, err error) {
		basename := filepath.Base(path)
		if err != nil {
			log.Printf("Error updating %s: %s", basename, err)
		} else {
			log.Printf("File %s updated successfully.", basename)
		}
	})
	if err != nil {
		log.Fatal(err)
	}
}
