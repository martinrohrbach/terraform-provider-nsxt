/* Copyright © 2023 VMware, Inc. All Rights Reserved.
   SPDX-License-Identifier: MPL-2.0 */

package nsxt

import (
	"fmt"
	"log"
	"math/rand"
	"net"
	"strings"
	"time"

	"golang.org/x/exp/slices"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/vmware/vsphere-automation-sdk-go/services/nsxt-mp/nsx"
	nsxModel "github.com/vmware/vsphere-automation-sdk-go/services/nsxt-mp/nsx/model"
)

func resourceNsxtManagerCluster() *schema.Resource {
	return &schema.Resource{
		Create: resourceNsxtManagerClusterCreate,
		Read:   resourceNsxtManagerClusterRead,
		Update: resourceNsxtManagerClusterUpdate,
		Delete: resourceNsxtManagerClusterDelete,

		Schema: map[string]*schema.Schema{
			"revision": getRevisionSchema(),
			"node": {
				Type:        schema.TypeList,
				Description: "Nodes in the cluster",
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"id": {
							Type:        schema.TypeString,
							Description: "ID of the cluster node",
							Computed:    true,
						},
						"fqdn": {
							Type:        schema.TypeString,
							Description: "FQDN for the cluster node",
							Computed:    true,
						},
						"status": {
							Type:        schema.TypeString,
							Description: "Status of the cluster node",
							Computed:    true,
						},
						"ip_address": {
							Type:         schema.TypeString,
							Description:  "IP address of the cluster node that will join the cluster",
							ValidateFunc: validation.IsIPAddress,
							Required:     true,
						},
						"password": {
							Type:        schema.TypeString,
							Description: "The password for login",
							Required:    true,
							Sensitive:   true,
						},
						"username": {
							Type:        schema.TypeString,
							Description: "The username for login",
							Required:    true,
						},
					},
				},
				Required: true,
			},
		},
	}
}

type NsxClusterNode struct {
	ID        string
	IPAddress string
	UserName  string
	Password  string
	Fqdn      string
	Status    string
}

func getClusterNodesFromSchema(d *schema.ResourceData) []NsxClusterNode {
	nodes := d.Get("node").([]interface{})
	var clusterNodes []NsxClusterNode
	for _, node := range nodes {
		data := node.(map[string]interface{})
		id := data["id"].(string)
		ipAddress := data["ip_address"].(string)
		userName := data["username"].(string)
		password := data["password"].(string)
		nodeObj := NsxClusterNode{
			ID:        id,
			IPAddress: ipAddress,
			UserName:  userName,
			Password:  password,
		}
		clusterNodes = append(clusterNodes, nodeObj)
	}
	return clusterNodes
}

func resourceNsxtManagerClusterCreate(d *schema.ResourceData, m interface{}) error {
	// Call Joincluster function on nodes that are not in the cluster
	nodes := getClusterNodesFromSchema(d)
	if len(nodes) == 0 {
		return fmt.Errorf("At least a manager appliance must be provided to form a cluster")
	}
	clusterID, certSha256Thumbprint, hostIPs, err := getClusterInfoFromHostNode(d, m)
	if err != nil {
		return handleCreateError("ManagerCluster", "", err)
	}

	for _, guestNode := range nodes {
		err := joinNodeToCluster(clusterID, certSha256Thumbprint, guestNode, hostIPs, d, m)
		if err != nil {
			return handleCreateError("ManagerCluster", clusterID, fmt.Errorf("failed to join node %s: %s", guestNode.ID, err))
		}
	}
	d.SetId(clusterID)
	return resourceNsxtManagerClusterRead(d, m)
}

func getClusterInfoFromHostNode(d *schema.ResourceData, m interface{}) (string, string, []string, error) {
	// function return values are:
	// clusterID, certSha256Thumbprint, hostIP, error
	connector := getPolicyConnector(m)
	client := nsx.NewClusterClient(connector)
	c := m.(nsxtClients)
	min := c.CommonConfig.MinRetryInterval
	max := c.CommonConfig.MaxRetryInterval
	maxRetries := c.CommonConfig.MaxRetries
	hostIPs := []string{}
	for i := 0; i < maxRetries; i++ {
		clusterConfig, err := client.Get()
		if err != nil {
			return "", "", hostIPs, err
		}
		if len(hostIPs) == 0 {
			hostIPs, err = resolveHostIPs(m)
			if err != nil {
				return "", "", hostIPs, err
			}

			log.Printf("[DEBUG]: Host resolved to IP addresses %v", hostIPs)
		}
		clusterID := *clusterConfig.ClusterId
		nodes := clusterConfig.Nodes
		node := nodes[0]
		apiListenAddr := node.ApiListenAddr
		if apiListenAddr != nil {
			certSha256Thumbprint := *apiListenAddr.CertificateSha256Thumbprint
			return clusterID, certSha256Thumbprint, hostIPs, nil
		}
		interval := (rand.Intn(max-min) + min)
		time.Sleep(time.Duration(interval) * time.Millisecond)
		log.Printf("[DEBUG]: Waited %d ms before retrying getting API Listen Address, attempt %d", interval, i+1)
	}
	return "", "", hostIPs, fmt.Errorf("Failed to read ClusterConfig after %d attempts", maxRetries)
}

func resolveHostIPs(client interface{}) ([]string, error) {
	c := client.(nsxtClients)
	host := c.Host
	host = strings.TrimPrefix(host, "https://")
	// Check if host is ip address or fqdn, if it's ip address then we are all set
	// Otherwise we resolve the host
	ip := net.ParseIP(host)
	if ip != nil {
		return []string{host}, nil
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("Failed to resolve host ip from %s: %v", host, err)
	}

	var result []string
	for _, ip := range ips {
		result = append(result, ip.String())
	}

	return result, nil
}

func getNewNsxtClient(node NsxClusterNode, d *schema.ResourceData, clients interface{}) (interface{}, error) {
	// The API of joining a new node to cluster needs to be called with the new node's ip,
	// to do that we initialize a new nsxtClient with new node's ip as its host
	username := node.UserName
	password := node.Password
	host := node.IPAddress
	if !strings.HasPrefix(host, "https://") {
		host = fmt.Sprintf("https://%s", host)
	}
	c := clients.(nsxtClients)
	commonConfig := c.CommonConfig
	newClients := nsxtClients{
		CommonConfig: commonConfig,
	}
	err := configureNewClient(&newClients, &c, host, username, password)
	if err != nil {
		return nil, err
	}
	return newClients, nil
}

func configureNewClient(newClient *nsxtClients, oldClient *nsxtClients, host string, username string, password string) error {
	newClient.Host = host
	securityCtx, err := getConfiguredSecurityContext(newClient, "", "", "", username, password)
	if err != nil {
		return fmt.Errorf("Failed to configure new client with host %s: %s", host, err)
	}
	newClient.PolicySecurityContext = securityCtx
	newClient.PolicyHTTPClient = oldClient.PolicyHTTPClient
	err = initNSXVersion(getPolicyConnector(*newClient))
	if err != nil {
		return fmt.Errorf("Failed to configure new client with host %s: %s", host, err)
	}
	return nil
}

func joinNodeToCluster(clusterID string, certSha256Thumbprint string, guestNode NsxClusterNode, hostIPs []string, d *schema.ResourceData, m interface{}) error {
	c, err := getNewNsxtClient(guestNode, d, m)
	if err != nil {
		return err
	}
	log.Printf("[INFO] Cluster %s. Joining node %s", clusterID, guestNode.IPAddress)
	newNsxClients := c.(nsxtClients)
	connector := getPolicyConnector(newNsxClients)
	client := nsx.NewClusterClient(connector)
	username, password := getHostCredential(m)
	hostIP := getMatchingIPVersion(guestNode.IPAddress, hostIPs)
	if hostIP == "" {
		return fmt.Errorf("[ERROR] Failed to find matching IP version for the host in IP list %v", hostIPs)
	}
	joinClusterParams := nsxModel.JoinClusterParameters{
		CertificateSha256Thumbprint: &certSha256Thumbprint,
		ClusterId:                   &clusterID,
		IpAddress:                   &hostIP,
		Username:                    &username,
		Password:                    &password,
	}
	_, err = client.Joincluster(joinClusterParams)
	if err != nil {
		return fmt.Errorf("Failed to join node to cluster: %s, node ip address: %s", err, guestNode.IPAddress)
	}
	log.Printf("[INFO] Cluster %s. Completed join node %s", clusterID, guestNode.IPAddress)
	return nil
}

func getHostCredential(m interface{}) (string, string) {
	nsxtClient := m.(nsxtClients)
	username := nsxtClient.CommonConfig.Username
	password := nsxtClient.CommonConfig.Password
	return username, password
}

func getMatchingIPVersion(ip string, hostIPs []string) string {
	needIPv4 := (net.ParseIP(ip)).To4() != nil

	for _, hostIP := range hostIPs {
		isIPv4 := (net.ParseIP(hostIP)).To4() != nil
		if needIPv4 == isIPv4 {
			// we return hostIP if either node ip is v4 and current host is v4,
			// or node ip is v4 and current resolved host is v6
			return hostIP
		}
	}

	return ""
}

func isMatchingNode(node nsxModel.ClusterNodeInfo, address string) bool {
	addr := net.ParseIP(address)
	for _, entity := range node.Entities {
		if entity.Port != nil {
			if entity.IpAddress != nil {
				nodeAddr := net.ParseIP(*entity.IpAddress)
				if nodeAddr.Equal(addr) {
					return true
				}
			}
			if entity.Ipv6Address != nil {
				nodeAddr := net.ParseIP(*entity.Ipv6Address)
				if nodeAddr.Equal(addr) {
					return true
				}
			}
		}
	}

	return false
}

func resourceNsxtManagerClusterRead(d *schema.ResourceData, m interface{}) error {
	id := d.Id()
	connector := getPolicyConnector(m)
	client := nsx.NewClusterClient(connector)
	clusterConfig, err := client.Get()
	if err != nil {
		return handleReadError(d, "ManagerCluster", id, err)
	}
	nsxNodes := clusterConfig.Nodes
	var resultNodes []map[string]interface{}
	schemaNodes := getClusterNodesFromSchema(d)
	// Complete schema nodes with computed fields
	for _, schemaNode := range schemaNodes {
		for _, nsxNode := range nsxNodes {
			if isMatchingNode(nsxNode, schemaNode.IPAddress) {
				resultNode := make(map[string]interface{})
				resultNode["id"] = nsxNode.NodeUuid
				resultNode["fqdn"] = nsxNode.Fqdn
				resultNode["status"] = nsxNode.Status
				resultNode["ip_address"] = schemaNode.IPAddress
				resultNode["username"] = schemaNode.UserName
				resultNode["password"] = schemaNode.Password

				resultNodes = append(resultNodes, resultNode)
			}
		}

	}

	d.Set("revision", clusterConfig.Revision)
	d.Set("node", resultNodes)
	return nil
}

func resourceNsxtManagerClusterUpdate(d *schema.ResourceData, m interface{}) error {
	id := d.Id()
	connector := getPolicyConnector(m)
	client := nsx.NewClusterClient(connector)

	clusterID, certSha256Thumbprint, hostIPs, err := getClusterInfoFromHostNode(d, m)
	if err != nil {
		return handleUpdateError("ManagerCluster", id, err)
	}
	oldNodes, newNodes := d.GetChange("node")
	oldNodesIPs := getClusterNodesIPs(oldNodes)
	newNodesIPs := getClusterNodesIPs(newNodes)
	for _, node := range oldNodes.([]interface{}) {
		nodeMap := node.(map[string]interface{})
		ip := nodeMap["ip_address"].(string)
		if !slices.Contains(newNodesIPs, ip) {
			id := nodeMap["id"].(string)
			force := "true"
			gracefulShutdown := "true"
			ignoreRepositoryIPCheckParam := "false"
			_, err := client.Removenode(id, &force, &gracefulShutdown, &ignoreRepositoryIPCheckParam)
			if err != nil {
				return handleUpdateError("ManagerCluster", id, err)
			}
		}
	}
	for _, node := range newNodes.([]interface{}) {
		nodeMap := node.(map[string]interface{})
		ip := nodeMap["ip_address"].(string)
		if !slices.Contains(oldNodesIPs, ip) {
			userName := nodeMap["username"].(string)
			password := nodeMap["password"].(string)
			ip := nodeMap["ip_address"].(string)
			nodeObj := NsxClusterNode{
				IPAddress: ip,
				UserName:  userName,
				Password:  password,
			}
			err = joinNodeToCluster(clusterID, certSha256Thumbprint, nodeObj, hostIPs, d, m)
			if err != nil {
				return handleUpdateError("ManagerCluster", id, err)
			}
		}
	}

	return resourceNsxtManagerClusterRead(d, m)
}

func getClusterNodesIPs(nodes interface{}) []string {
	var ips []string
	for _, node := range nodes.([]interface{}) {
		nodeMap := node.(map[string]interface{})
		ips = append(ips, nodeMap["ip_address"].(string))
	}
	return ips
}

func resourceNsxtManagerClusterDelete(d *schema.ResourceData, m interface{}) error {
	connector := getPolicyConnector(m)
	client := nsx.NewClusterClient(connector)
	nodes := getClusterNodesFromSchema(d)
	force := "true"
	gracefulShutdown := "true"
	ignoreRepositoryIPCheckParam := "false"
	for _, node := range nodes {
		guestNodeID := node.ID
		_, err := client.Removenode(guestNodeID, &force, &gracefulShutdown, &ignoreRepositoryIPCheckParam)
		if err != nil {
			return handleDeleteError("ManagerCluster", guestNodeID, err)
		}
	}
	return nil
}
