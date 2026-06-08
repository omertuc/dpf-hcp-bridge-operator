package dpuworkerconfigure

import (
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"os"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	bridgeName = "br-dpu"
	ipHintFile = "/run/nodeip-configuration/primary-ip"
)

// runSetup performs one-shot DPU worker node configuration:
// 1. Creates br-dpu Linux bridge via nmstatectl
// 2. Unmanages ovn-k8s-* interfaces in NetworkManager
// 3. Disables/masks OVS services
func runSetup() error {
	log := ctrl.Log.WithName("setup")

	// Bridge setup
	log.Info("Starting bridge setup")
	if err := setupBridge(); err != nil {
		return fmt.Errorf("bridge setup: %w", err)
	}

	// NM unmanage ovn-k8s-* interfaces
	log.Info("Unmanaging ovn-k8s-* interfaces in NetworkManager")
	if err := unmanageOVNKInterfaces(); err != nil {
		log.Error(err, "Failed to unmanage ovn-k8s interfaces (may not exist yet)")
	}

	// Disable OVS services
	log.Info("Disabling OVS services")
	if err := disableOVSServices(); err != nil {
		return fmt.Errorf("disabling OVS services: %w", err)
	}

	log.Info("Setup completed successfully")
	return nil
}

func readNodeIP() (string, error) {
	// nsenter into host to read the file
	data, err := nsenterRun("cat", ipHintFile)
	if err != nil {
		return "", fmt.Errorf("reading IP hint file %s: %w", ipHintFile, err)
	}

	ip := strings.TrimSpace(data)
	if ip == "" {
		return "", fmt.Errorf("IP hint file %s is empty", ipHintFile)
	}

	return ip, nil
}

func bridgeExists() bool {
	_, err := nsenterRun("ip", "link", "show", bridgeName)
	return err == nil
}

func waitForBridgeIP(nodeIP string, timeout time.Duration) error {
	log := ctrl.Log.WithName("setup")
	deadline := time.Now().Add(timeout)

	log.Info("Waiting for bridge to acquire IP", "bridge", bridgeName, "ip", nodeIP, "timeout", timeout)
	for time.Now().Before(deadline) {
		output, err := nsenterRun("ip", "-o", "addr", "show", "dev", bridgeName)
		if err == nil && strings.Contains(output, nodeIP) {
			log.Info("Bridge has acquired IP", "bridge", bridgeName, "ip", nodeIP)
			return nil
		}
		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("bridge %s did not acquire %s within %s", bridgeName, nodeIP, timeout)
}

func setRPFilterLoose() error {
	_, err := nsenterRun("sysctl", "-w", fmt.Sprintf("net.ipv4.conf.%s.rp_filter=2", bridgeName))
	return err
}

// findInterfaceWithIP finds the network interface that holds the given IP, excluding the bridge.
func findInterfaceWithIP(nodeIP string) (string, error) {
	output, err := nsenterRun("ip", "-j", "addr", "show")
	if err != nil {
		return "", fmt.Errorf("listing interfaces: %w", err)
	}

	var interfaces []struct {
		Ifname   string `json:"ifname"`
		AddrInfo []struct {
			Local string `json:"local"`
		} `json:"addr_info"`
	}

	if err := json.Unmarshal([]byte(output), &interfaces); err != nil {
		return "", fmt.Errorf("parsing interface JSON: %w", err)
	}

	for _, iface := range interfaces {
		if iface.Ifname == bridgeName {
			continue
		}
		for _, addr := range iface.AddrInfo {
			if addr.Local == nodeIP {
				return iface.Ifname, nil
			}
		}
	}

	return "", fmt.Errorf("no interface found with IP %s", nodeIP)
}

func buildNMStateConfig(physIface string, mtu string) ([]byte, error) {
	// Get current interface state from nmstatectl
	ifaceJSON, err := nsenterRun("nmstatectl", "show", physIface, "--json")
	if err != nil {
		return nil, fmt.Errorf("getting interface state: %w", err)
	}

	var ifaceState struct {
		Interfaces []map[string]any `json:"interfaces"`
	}
	if err := json.Unmarshal([]byte(ifaceJSON), &ifaceState); err != nil {
		return nil, fmt.Errorf("parsing interface state: %w", err)
	}
	if len(ifaceState.Interfaces) == 0 {
		return nil, fmt.Errorf("no interface data returned for %s", physIface)
	}
	phys := ifaceState.Interfaces[0]

	// Get existing routes on the physical interface
	routesJSON, err := nsenterRun("nmstatectl", "show", "--json")
	if err != nil {
		return nil, fmt.Errorf("getting nmstate full state: %w", err)
	}

	var fullState struct {
		Routes struct {
			Config []map[string]any `json:"config"`
		} `json:"routes"`
	}
	if err := json.Unmarshal([]byte(routesJSON), &fullState); err != nil {
		return nil, fmt.Errorf("parsing routes: %w", err)
	}

	// Filter routes for this interface and rewrite to bridge
	var bridgeRoutes []map[string]any
	for _, route := range fullState.Routes.Config {
		if nhIface, ok := route["next-hop-interface"].(string); ok && nhIface == physIface {
			route["next-hop-interface"] = bridgeName
			bridgeRoutes = append(bridgeRoutes, route)
		}
	}

	// Build bridge interface
	bridgeIface := map[string]any{
		"name":  bridgeName,
		"type":  "linux-bridge",
		"state": "up",
	}
	if mac, ok := phys["mac-address"]; ok {
		bridgeIface["mac-address"] = mac
	}
	if ipv4, ok := phys["ipv4"].(map[string]any); ok {
		ipv4Copy := copyMap(ipv4)
		delete(ipv4Copy, "forwarding")
		bridgeIface["ipv4"] = ipv4Copy
	}
	if ipv6, ok := phys["ipv6"].(map[string]any); ok {
		ipv6Copy := copyMap(ipv6)
		delete(ipv6Copy, "forwarding")
		bridgeIface["ipv6"] = ipv6Copy
	}
	bridgeIface["bridge"] = map[string]any{
		"options": map[string]any{
			"stp": map[string]any{"enabled": false},
		},
		"port": []map[string]any{
			{"name": physIface},
		},
	}
	if mtu != "" {
		bridgeIface["mtu"] = mtu
	}

	// Build physical interface (strip IP)
	physIfaceSpec := map[string]any{
		"name":  physIface,
		"type":  phys["type"],
		"state": "up",
		"ipv4":  map[string]any{"enabled": false},
		"ipv6":  map[string]any{"enabled": false},
	}
	if la, ok := phys["link-aggregation"]; ok {
		physIfaceSpec["link-aggregation"] = la
	}
	if mtu != "" {
		physIfaceSpec["mtu"] = mtu
	}

	desired := map[string]any{
		"interfaces": []any{bridgeIface, physIfaceSpec},
	}
	if len(bridgeRoutes) > 0 {
		desired["routes"] = map[string]any{
			"config": bridgeRoutes,
		}
	}

	return json.MarshalIndent(desired, "", "  ")
}

func setupBridge() error {
	log := ctrl.Log.WithName("setup")

	nodeIP, err := readNodeIP()
	if err != nil {
		return err
	}
	log.Info("Node IP from hint file", "ip", nodeIP)

	// Check if bridge already exists (idempotent)
	if bridgeExists() {
		log.Info("Bridge already exists, waiting for IP", "bridge", bridgeName)
		if err := waitForBridgeIP(nodeIP, 120*time.Second); err != nil {
			return err
		}
		return setRPFilterLoose()
	}

	// Find the interface holding the node IP
	physIface, err := findInterfaceWithIP(nodeIP)
	if err != nil {
		return err
	}
	log.Info("Found physical interface", "interface", physIface, "ip", nodeIP)

	// Build and apply NMState configuration
	mtu := os.Getenv("NETWORK_MTU")
	config, err := buildNMStateConfig(physIface, mtu)
	if err != nil {
		return fmt.Errorf("building NMState config: %w", err)
	}

	log.Info("Applying NMState configuration", "config", string(config))

	// Write config to temp file on host and apply
	if _, err := nsenterRun("bash", "-c", fmt.Sprintf("echo '%s' > /tmp/br-dpu-config.json && nmstatectl apply /tmp/br-dpu-config.json && rm -f /tmp/br-dpu-config.json", string(config))); err != nil {
		return fmt.Errorf("applying NMState config: %w", err)
	}

	log.Info("Bridge created successfully", "bridge", bridgeName)

	if err := waitForBridgeIP(nodeIP, 120*time.Second); err != nil {
		return err
	}

	return setRPFilterLoose()
}

func unmanageOVNKInterfaces() error {
	log := ctrl.Log.WithName("setup")

	output, err := nsenterRun("ip", "-j", "link", "show")
	if err != nil {
		return fmt.Errorf("listing links: %w", err)
	}

	var links []struct {
		Ifname string `json:"ifname"`
	}
	if err := json.Unmarshal([]byte(output), &links); err != nil {
		return fmt.Errorf("parsing links: %w", err)
	}

	for _, link := range links {
		if strings.HasPrefix(link.Ifname, "ovn-k8s-") {
			log.Info("Setting interface unmanaged", "interface", link.Ifname)
			if _, err := nsenterRun("nmcli", "device", "set", link.Ifname, "managed", "no"); err != nil {
				log.Error(err, "Failed to unmanage interface", "interface", link.Ifname)
			}
		}
	}

	return nil
}

func disableOVSServices() error {
	log := ctrl.Log.WithName("setup")

	services := []struct {
		name string
		mask bool
	}{
		{"ovs-configuration.service", false},
		{"openvswitch.service", true},
		{"wait-for-br-ex-up.service", false},
	}

	for _, svc := range services {
		log.Info("Disabling service", "service", svc.name, "mask", svc.mask)
		if _, err := nsenterRun("systemctl", "disable", svc.name); err != nil {
			log.Error(err, "Failed to disable service (may not exist)", "service", svc.name)
		}
		if svc.mask {
			if _, err := nsenterRun("systemctl", "mask", svc.name); err != nil {
				log.Error(err, "Failed to mask service", "service", svc.name)
			}
		}
		// Stop if running
		if _, err := nsenterRun("systemctl", "stop", svc.name); err != nil {
			log.V(1).Info("Service not running or failed to stop", "service", svc.name)
		}
	}

	return nil
}

func isIPv6(ip string) bool {
	return net.ParseIP(ip) != nil && strings.Contains(ip, ":")
}

func copyMap(m map[string]any) map[string]any {
	result := make(map[string]any, len(m))
	maps.Copy(result, m)
	return result
}
