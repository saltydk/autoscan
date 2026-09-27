package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"go.yaml.in/yaml/v3"
)

func decodeConfig(reader io.Reader) (config, error) {
	c := config{
		MinimumAge: 10 * time.Minute,
		ScanDelay:  5 * time.Second,
		ScanStats:  time.Hour,
		Host:       []string{""},
		Port:       3030,
	}
	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)
	err := decoder.Decode(&c)
	return c, err
}

func defaultConfigDirectory(app string, filename string) string {
	// binary path
	bcd := getBinaryPath()
	if _, err := os.Stat(filepath.Join(bcd, filename)); err == nil {
		// there is a config file in the binary path
		// so use this directory as the default
		return bcd
	}

	// config dir
	ucd, err := os.UserConfigDir()
	if err != nil {
		panic(fmt.Sprintf("userconfigdir: %v", err))
	}

	acd := filepath.Join(ucd, app)
	if _, err := os.Stat(acd); os.IsNotExist(err) {
		if err := os.MkdirAll(acd, os.ModePerm); err != nil {
			panic(fmt.Sprintf("mkdirall: %v", err))
		}
	}

	return acd
}

func getBinaryPath() string {
	// get current binary path
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		// get current working dir
		if dir, err = os.Getwd(); err != nil {
			panic(fmt.Sprintf("getwd: %v", err))
		}
	}

	return dir
}
