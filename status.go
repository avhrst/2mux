package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func statusSnapshot(dir string) (any, error) {
	messages, problems, err := scanMessages(dir)
	if err != nil {
		return nil, err
	}
	counts := map[messageStatus]int{}
	deliveryErrors := map[string]string{}
	for _, m := range messages {
		counts[m.Status]++
		if m.Error != "" {
			deliveryErrors[m.ID] = m.Error
		}
	}
	cfg, err := readSessionConfig(dir)
	if err != nil {
		return nil, err
	}
	return struct {
		Directory        string                `json:"directory"`
		Bridge           string                `json:"bridge"`
		Config           sessionConfig         `json:"config"`
		Transports       map[string]string     `json:"transports"`
		Agents           []agentState          `json:"agents"`
		Messages         map[messageStatus]int `json:"messages"`
		Problems         []string              `json:"problems,omitempty"`
		ChannelConnected bool                  `json:"channel_connected"`
		DeliveryErrors   map[string]string     `json:"delivery_errors,omitempty"`
	}{dir, bridgeDescription(dir), cfg, configuredTransports(cfg), []agentState{observedAgentState(dir, roleWorker), observedAgentState(dir, roleReviewer)}, counts, problems, channelAvailable(dir), deliveryErrors}, nil
}
func printStatusJSON(dir string) error {
	s, err := statusSnapshot(dir)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(s)
}
func watchStatus(dir string) error {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var previous string
	for {
		s, err := statusSnapshot(dir)
		if err != nil {
			return err
		}
		data, err := json.Marshal(s)
		if err != nil {
			return err
		}
		if string(data) != previous {
			fmt.Println(string(data))
			previous = string(data)
		}
		select {
		case <-stop:
			return nil
		case <-ticker.C:
		}
	}
}
