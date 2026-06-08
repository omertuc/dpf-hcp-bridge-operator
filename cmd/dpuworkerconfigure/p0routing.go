package dpuworkerconfigure

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	checkInterval     = 2 * time.Second
	reconcileInterval = 60 * time.Second
	routingTable      = "100"
)

// runP0Routing is a long-running process that maintains routing table 100
// for node-originated traffic via br-dpu. It continuously monitors and
// re-applies rules/routes in case they are removed.
func runP0Routing() error {
	log := ctrl.Log.WithName("p0-routing")

	nodeIP, err := readNodeIP()
	if err != nil {
		return err
	}
	log.Info("Using node IP", "ip", nodeIP)

	ipv6 := isIPv6(nodeIP)
	ipFlag := "-4"
	prefixLen := "32"
	linkLocalPattern := "169.254."
	if ipv6 {
		ipFlag = "-6"
		prefixLen = "128"
		linkLocalPattern = "fe80:"
	}
	log.Info("Detected IP version", "ipv6", ipv6)

	log.Info("Waiting for OVN-K interface to get an IP address")

	configured := false
	for {
		ovnkIface, ovnkIP, found := findOVNKInterface(linkLocalPattern)
		if found {
			if !configured {
				log.Info("Found OVN-K interface", "interface", ovnkIface, "ip", ovnkIP)
			}
			if err := configureRouting(ipFlag, nodeIP, prefixLen, ovnkIface); err != nil {
				if !configured {
					log.Error(err, "Routing configuration failed, will retry")
				}
			} else if !configured {
				log.Info("Routing configuration completed, entering reconcile loop")
				configured = true
			}
		}

		if configured {
			time.Sleep(reconcileInterval)
		} else {
			time.Sleep(checkInterval)
		}
	}
}

// findOVNKInterface finds an OVN-K interface with a non-link-local IP.
func findOVNKInterface(linkLocalPattern string) (ifname, ip string, found bool) {
	output, err := nsenterRun("ip", "-j", "addr", "show")
	if err != nil {
		return "", "", false
	}

	var interfaces []struct {
		Ifname   string `json:"ifname"`
		AddrInfo []struct {
			Local string `json:"local"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(output), &interfaces); err != nil {
		return "", "", false
	}

	for _, iface := range interfaces {
		hasLinkLocal := false
		for _, addr := range iface.AddrInfo {
			if strings.HasPrefix(addr.Local, linkLocalPattern) {
				hasLinkLocal = true
				break
			}
		}
		if !hasLinkLocal {
			continue
		}

		// Found an interface with link-local, now check for non-link-local IP
		for _, addr := range iface.AddrInfo {
			if addr.Local != "" && !strings.HasPrefix(addr.Local, linkLocalPattern) && !strings.HasPrefix(addr.Local, "fe80:") && !strings.HasPrefix(addr.Local, "169.254.") {
				return iface.Ifname, addr.Local, true
			}
		}
	}

	return "", "", false
}

func configureRouting(ipFlag, nodeIP, prefixLen, ovnkIface string) error {
	// Get br-dpu network
	brDPUNetwork, err := getBrDPUNetwork(ipFlag)
	if err != nil {
		return fmt.Errorf("getting br-dpu network: %w", err)
	}

	// Get br-dpu gateway
	brDPUGateway, err := getBrDPUGateway(ipFlag)
	if err != nil {
		return fmt.Errorf("getting br-dpu gateway: %w", err)
	}

	// Get OVN-K subnet
	ovnkSubnet, err := getOVNKSubnet(ipFlag, ovnkIface)
	if err != nil {
		return fmt.Errorf("getting OVN-K subnet: %w", err)
	}

	// Get br-dpu metric
	brDPUMetric := getBrDPUMetric(ipFlag)

	// Ensure policy routing rule
	if err := ensureRule(ipFlag, nodeIP, prefixLen); err != nil {
		return fmt.Errorf("ensuring rule: %w", err)
	}

	// Ensure OVN-K subnet route in table 100
	if err := ensureRoute(ipFlag, ovnkSubnet, "via", brDPUGateway); err != nil {
		return fmt.Errorf("ensuring OVN-K route: %w", err)
	}

	// Ensure br-dpu network route in table 100
	if err := ensureRoute(ipFlag, brDPUNetwork, "dev", bridgeName, "proto", "kernel", "scope", "link", "src", nodeIP, "metric", brDPUMetric); err != nil {
		return fmt.Errorf("ensuring br-dpu route: %w", err)
	}

	return nil
}

func ensureRule(ipFlag, nodeIP, prefixLen string) error {
	output, err := nsenterRun("ip", ipFlag, "-j", "rule", "list")
	if err != nil {
		return err
	}

	var rules []struct {
		Src   string `json:"src"`
		Table string `json:"table"`
	}
	if err := json.Unmarshal([]byte(output), &rules); err != nil {
		return fmt.Errorf("parsing rules: %w", err)
	}

	for _, rule := range rules {
		if rule.Src == nodeIP && rule.Table == routingTable {
			return nil
		}
	}

	log := ctrl.Log.WithName("p0-routing")
	log.Info("Adding rule", "from", fmt.Sprintf("%s/%s", nodeIP, prefixLen), "table", routingTable)
	_, err = nsenterRun("ip", ipFlag, "rule", "add", "from", fmt.Sprintf("%s/%s", nodeIP, prefixLen), "lookup", routingTable)
	return err
}

func ensureRoute(ipFlag string, dst string, routeArgs ...string) error {
	output, err := nsenterRun("ip", ipFlag, "-j", "route", "show", "table", routingTable)
	if err != nil {
		// Table may not exist yet, that's fine
		output = "[]"
	}

	var routes []struct {
		Dst string `json:"dst"`
	}
	if err := json.Unmarshal([]byte(output), &routes); err != nil {
		return fmt.Errorf("parsing routes: %w", err)
	}

	for _, route := range routes {
		if route.Dst == dst {
			return nil
		}
	}

	log := ctrl.Log.WithName("p0-routing")
	log.Info("Adding route", "dst", dst, "table", routingTable, "args", routeArgs)
	args := []string{ipFlag, "route", "add", dst}
	args = append(args, routeArgs...)
	args = append(args, "table", routingTable)
	_, err = nsenterRun("ip", args...)
	return err
}

func getBrDPUNetwork(ipFlag string) (string, error) {
	output, err := nsenterRun("ip", ipFlag, "-j", "route", "show", "dev", bridgeName)
	if err != nil {
		return "", err
	}

	var routes []struct {
		Dst      string `json:"dst"`
		Protocol string `json:"protocol"`
	}
	if err := json.Unmarshal([]byte(output), &routes); err != nil {
		return "", err
	}

	for _, route := range routes {
		if route.Protocol == "kernel" {
			return route.Dst, nil
		}
	}

	return "", fmt.Errorf("no kernel route found for %s", bridgeName)
}

func getBrDPUGateway(ipFlag string) (string, error) {
	output, err := nsenterRun("ip", ipFlag, "-j", "route")
	if err != nil {
		return "", err
	}

	var routes []struct {
		Dst     string `json:"dst"`
		Dev     string `json:"dev"`
		Gateway string `json:"gateway"`
	}
	if err := json.Unmarshal([]byte(output), &routes); err != nil {
		return "", err
	}

	for _, route := range routes {
		if route.Dst == "default" && route.Dev == bridgeName {
			return route.Gateway, nil
		}
	}

	return "", fmt.Errorf("no default gateway found for %s", bridgeName)
}

func getOVNKSubnet(ipFlag, ovnkIface string) (string, error) {
	output, err := nsenterRun("ip", ipFlag, "-j", "route")
	if err != nil {
		return "", err
	}

	var routes []struct {
		Dst     string `json:"dst"`
		Dev     string `json:"dev"`
		Gateway string `json:"gateway"`
	}
	if err := json.Unmarshal([]byte(output), &routes); err != nil {
		return "", err
	}

	for _, route := range routes {
		if route.Dev != ovnkIface || route.Dst == "" || route.Dst == "default" {
			continue
		}
		if route.Gateway != "" {
			continue
		}
		// Skip link-local
		if strings.HasPrefix(route.Dst, "169.254.") || strings.HasPrefix(route.Dst, "fe80:") {
			continue
		}
		return route.Dst, nil
	}

	return "", fmt.Errorf("no subnet found for %s", ovnkIface)
}

func getBrDPUMetric(ipFlag string) string {
	output, err := nsenterRun("ip", ipFlag, "-j", "route", "show", "dev", bridgeName)
	if err != nil {
		return "425"
	}

	var routes []struct {
		Protocol string `json:"protocol"`
		Metric   int    `json:"metric"`
	}
	if err := json.Unmarshal([]byte(output), &routes); err != nil {
		return "425"
	}

	for _, route := range routes {
		if route.Protocol == "kernel" {
			if route.Metric > 0 {
				return fmt.Sprintf("%d", route.Metric)
			}
			return "425"
		}
	}

	return "425"
}
