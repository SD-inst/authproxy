package main

import (
	"flag"
	"log"
	"path/filepath"

	"github.com/rkfg/authproxy/civitai"
)

func main() {
	var pageOnly bool
	flag.BoolVar(&pageOnly, "page", false, "only fill the \"model page\" field in existing JSON files, looking the model up on CivitAI by hash")
	flag.Parse()
	if len(flag.Args()) != 1 {
		log.Fatal("Provide the model path as the only argument")
	}
	dl := civitai.NewDownloader()
	root := flag.Args()[0]
	var perFile func(path string) error
	var msg func(path string, err error)
	if pageOnly {
		perFile = dl.UpdateModelPage
		msg = func(path string, err error) {
			basename := filepath.Base(path)
			if err != nil {
				log.Printf("Error updating %s: %s", basename, err)
			} else {
				log.Printf("File %s checked.", basename)
			}
		}
	} else {
		perFile = dl.UpdateFile
		msg = func(path string, err error) {
			basename := filepath.Base(path)
			if err != nil {
				log.Printf("Error updating %s: %s", basename, err)
			} else {
				log.Printf("File %s updated successfully.", basename)
			}
		}
	}
	if err := dl.Walk(root, perFile, msg); err != nil {
		log.Fatal(err)
	}
}
