package main

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/wltechblog/gino/internal/config"
)

func TestConfigureLoggingOffSilencesStream(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	cfg := config.Config{}
	cfg.Agents.Defaults.LogLevel = "off"
	configureLogging(cfg)

	log.Printf("routine message that must be discarded")
	if buf.Len() != 0 {
		t.Fatalf("expected silenced output, got %q", buf.String())
	}
}

func TestConfigureLoggingInfoKeepsStream(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	cfg := config.Config{}
	cfg.Agents.Defaults.LogLevel = "info"
	configureLogging(cfg)

	log.Printf("routine message kept")
	if !strings.Contains(buf.String(), "routine message kept") {
		t.Fatal("info level must keep log output")
	}
}
