// Copyright 2016 Apcera Inc. All rights reserved.

// Package arm provides methods for creating and manipulating VMs on Azure using arm API.
package arm

import (
	"errors"
	"net"
	"strings"
	"time"

	"github.com/apcera/libretto/ssh"
	"github.com/apcera/libretto/util"
	lvm "github.com/apcera/libretto/virtualmachine"

	"github.com/Azure/azure-sdk-for-go/arm/compute"
	"github.com/Azure/go-autorest/autorest/azure"
)

var (
	// ErrActionTimeout is returned when the Azure instance takes too long to enter waited state.
	ErrActionTimeout = errors.New("Azure action timeout")
)

const (
	// PublicIP is the index of the public IP address that GetIPs returns.
	PublicIP = 0

	// PrivateIP is the index of the private IP address that GetIPs returns.
	PrivateIP = 1

	// sshTimeout is the maximum seconds to wait before failing to GetSSH.
	sshTimeout = 180

	// actionTimeout is the maximum seconds to wait before failing to
	// any action on VM, such as Provision, Halt or Destroy.
	actionTimeout = 90

	// running is the status returned when the VM is running
	running = "VM running"

	// stopped is the status returned when the VM is halted
	stopped = "VM stopped"

	// succeeded is the status returned when a deployment ends successfully
	succeeded = "Succeeded"

	// maxPublicIPLength is the maximum length that public ip can have
	maxPublicIPLength = 63
)

var _ lvm.VirtualMachine = (*VM)(nil)

// OAuthCredentials is the struct that stors OAUTH credentials
type OAuthCredentials struct {
	ClientID       string
	ClientSecret   string
	TenantID       string
	SubscriptionID string
}

// VM represents an Azure virtual machine.
type VM struct {
	// Credentials to connect Azure
	Creds OAuthCredentials

	// Image Properties
	ImagePublisher string
	ImageOffer     string
	ImageSku       string

	// VM Properties
	Size string
	Name string

	// SSH Properties
	SSHCreds ssh.Credentials // required

	// Deployment Properties
	ResourceGroup    string
	StorageAccount   string
	StorageContainer string

	// VM OS Properties
	OsFile string

	// VM Network Properties
	NetworkSecurityGroup string
	Nic                  string
	PublicIP             string
	Subnet               string
	VirtualNetwork       string

	// deployment
	DeploymentName string
}

// GetName returns the name of the VM.
func (vm *VM) GetName() string {
	return vm.Name
}

// Provision creates a new VM instance on Azure. It returns an error if there
// was a problem during creation.
func (vm *VM) Provision() error {
	// Validate VM
	err := validateVM(vm)
	if err != nil {
		return err
	}

	publicIPLength := len(vm.PublicIP)
	if publicIPLength > maxPublicIPLength {
		vm.PublicIP = vm.PublicIP[publicIPLength-maxPublicIPLength:]
	}

	// Create and send the deployment
	vm.deploy()

	// Use GetSSH to try to connect to machine
	cli, err := vm.GetSSH(ssh.Options{KeepAlive: 2})
	if err != nil {
		return err
	}

	return cli.WaitForSSH(sshTimeout * time.Second)
}

// GetIPs returns the IP addresses of the Azure VM instance.
func (vm *VM) GetIPs() ([]net.IP, error) {
	ips := make([]net.IP, 2)

	// Set up the authorizer
	authorizer, err := getServicePrincipalToken(&vm.Creds, azure.PublicCloud.ResourceManagerEndpoint)
	if err != nil {
		return nil, err
	}

	// Get the Public IP
	ip, err := vm.getPublicIP(authorizer)
	if err != nil {
		return nil, err
	}
	ips[PublicIP] = ip

	// Get the Private IP
	ip, err = vm.getPrivateIP(authorizer)
	if err != nil {
		return nil, err
	}
	ips[PrivateIP] = ip

	return ips, nil
}

// GetSSH returns an SSH client that can be used to connect to the VM. An error
// is returned if the VM has no IPs.
func (vm *VM) GetSSH(options ssh.Options) (ssh.Client, error) {
	ips, err := util.GetVMIPs(vm, options)
	if err != nil {
		return nil, err
	}

	client := ssh.SSHClient{
		Creds:   &vm.SSHCreds,
		IP:      ips[PublicIP],
		Options: options,
		Port:    22,
	}
	return &client, nil
}

// GetState returns the status of the Azure VM. The status will be one of the
// following:
//     "running"
//     "stopped"
func (vm *VM) GetState() (string, error) {
	// Set up the authorizer
	authorizer, err := getServicePrincipalToken(&vm.Creds, azure.PublicCloud.ResourceManagerEndpoint)
	if err != nil {
		return "", err
	}

	virtualMachinesClient := compute.NewVirtualMachinesClient(vm.Creds.SubscriptionID)
	virtualMachinesClient.Authorizer = authorizer

	r, e := virtualMachinesClient.Get(vm.ResourceGroup, vm.Name, "InstanceView")
	if r.Properties != nil && r.Properties.InstanceView != nil {
		state := *(*r.Properties.InstanceView.Statuses)[1].DisplayStatus
		return translateState(state), e
	}
	return "", e
}

// Destroy deletes the VM on Azure.
func (vm *VM) Destroy() error {
	// Set up the authorizer
	authorizer, err := getServicePrincipalToken(&vm.Creds, azure.PublicCloud.ResourceManagerEndpoint)
	if err != nil {
		return err
	}

	// Delete the VM
	virtualMachinesClient := compute.NewVirtualMachinesClient(vm.Creds.SubscriptionID)
	virtualMachinesClient.Authorizer = authorizer

	_, err = virtualMachinesClient.Delete(vm.ResourceGroup, vm.Name, nil)
	if err != nil {
		return err
	}

	// Make sure VM is deleted
	deleted := false
	for i := 0; i < actionTimeout; i++ {
		_, err := vm.GetState()
		if err != nil {
			if strings.Contains(err.Error(), `Code="ResourceNotFound"`) ||
				strings.Contains(err.Error(), `Code="NotFound"`) {
				deleted = true
				break
			}
			return err
		}

		time.Sleep(1 * time.Second)
	}

	if !deleted {
		return ErrActionTimeout
	}

	// Delete the OS File of this VM
	err = vm.deleteOSFile(authorizer)
	if err != nil {
		return err
	}

	// Delete the network interface of this VM
	err = vm.deleteNic(authorizer)
	if err != nil {
		return err
	}

	// Delete the public IP of this VM
	return vm.deletePublicIP(authorizer)
}

// Halt shuts down the VM.
func (vm *VM) Halt() error {
	// Set up the authorizer
	authorizer, err := getServicePrincipalToken(&vm.Creds, azure.PublicCloud.ResourceManagerEndpoint)
	if err != nil {
		return err
	}

	// Poweroff the VM
	virtualMachinesClient := compute.NewVirtualMachinesClient(vm.Creds.SubscriptionID)
	virtualMachinesClient.Authorizer = authorizer

	_, err = virtualMachinesClient.PowerOff(vm.ResourceGroup, vm.Name, nil)
	if err != nil {
		return err
	}

	// Make sure the VM is stopped
	for i := 0; i < actionTimeout; i++ {
		state, err := vm.GetState()
		if err != nil {
			return err
		}
		if state == lvm.VMHalted {
			return nil
		}

		time.Sleep(1 * time.Second)
	}
	return ErrActionTimeout
}

// Start boots a stopped VM.
func (vm *VM) Start() error {
	// Set up the authorizer
	authorizer, err := getServicePrincipalToken(&vm.Creds, azure.PublicCloud.ResourceManagerEndpoint)
	if err != nil {
		return err
	}

	// Start the VM
	virtualMachinesClient := compute.NewVirtualMachinesClient(vm.Creds.SubscriptionID)
	virtualMachinesClient.Authorizer = authorizer

	_, err = virtualMachinesClient.Start(vm.ResourceGroup, vm.Name, nil)
	if err != nil {
		return err
	}

	// Make sure the VM is running
	for i := 0; i < actionTimeout; i++ {
		state, err := vm.GetState()
		if err != nil {
			return err
		}
		if state == lvm.VMRunning {
			return nil
		}

		time.Sleep(1 * time.Second)
	}
	return ErrActionTimeout
}

// Suspend returns an error because it is not supported on Azure.
func (vm *VM) Suspend() error {
	return lvm.ErrSuspendNotSupported
}

// Resume returns an error because it is not supported on Azure.
func (vm *VM) Resume() error {
	return lvm.ErrResumeNotSupported
}
