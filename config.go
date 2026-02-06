package main

import (
	"os"

	"gopkg.in/yaml.v3"
)

type ACL map[string][]string

type Config struct {
	CredFilename string `yaml:"accounts" description:"Credentials filename" required:"true"`
	Domain       string `yaml:"domain" description:"Main domain"`
	Address      string `yaml:"address" description:"Listen at this address"`
	LoRAPath     string `yaml:"lora_uploads" description:"Path to the directory for LoRA uploads"`
	LoginHeader  string `yaml:"login_header" description:"Title text for login page"`
	LoginTitle   string `yaml:"login_title" description:"Login page invitation text"`
	SDTimeout    int    `yaml:"sd_timeout" description:"SD task timeout in seconds"`
	FIFOPath     string `yaml:"fifo_path" description:"Path to FIFO controlling instance restarts"`
	CookieFile   string `yaml:"cookie_file" description:"Path to the cookie storage file"`
	PushPassword string `yaml:"push_password" description:"Password to push prometheus metrics from other services"`
	StaticPath   string `yaml:"static_path" description:"Path to the static pages (each dir will be available at corresponding /dir URL)"`
	ACL          ACL    `yaml:"acl,flow" description:"Mapping of user names to a list or roles or * for full access"`
}

var config = Config{
	Address:     "0.0.0.0:8000",
	LoginHeader: "Stable Diffusion for friends",
	LoginTitle:  "Please log in",
	SDTimeout:   300,
	FIFOPath:    "/var/run/sdwd/control.fifo",
	CookieFile:  "cookie.txt",
}

func loadConfig(filename string) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	return yaml.NewDecoder(f).Decode(&config)
}
