/*
Copyright 2020 KubeSphere Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ipam

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"reflect"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/davecgh/go-spew/spew"
	cnet "github.com/projectcalico/libcalico-go/lib/net"
	"github.com/projectcalico/libcalico-go/lib/set"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/yunify/hostnic-cni/pkg/apis/network/v1alpha1"
	networkv1alpha1 "github.com/yunify/hostnic-cni/pkg/apis/network/v1alpha1"
	"github.com/yunify/hostnic-cni/pkg/client/clientset/versioned"
	"github.com/yunify/hostnic-cni/pkg/client/clientset/versioned/scheme"
	"github.com/yunify/hostnic-cni/pkg/simple/client/network/utils"
)

const (
	// Number of retries when we have an error writing data to etcd.
	datastoreRetries = 10
)

var (
	ErrNoQualifiedPool  = errors.New("cannot find a qualified ippool")
	ErrNoFreeBlocks     = errors.New("no free blocks in ippool")
	ErrMaxRetry         = errors.New("Max retries hit - excessive concurrent IPAM requests")
	ErrUnknowIPPoolType = errors.New("unknow ippool type")
)

func (c IPAMClient) getAllPools() ([]v1alpha1.IPPool, error) {
	pools, err := c.client.NetworkV1alpha1().IPPools().List(context.Background(), metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{
			v1alpha1.IPPoolTypeLabel: c.typeStr,
		}).String(),
	})

	if err != nil {
		return nil, err
	}

	return pools.Items, nil
}

// NewIPAMClient returns a new IPAMClient, which implements Interface.
func NewIPAMClient(client versioned.Interface, typeStr string) IPAMClient {
	return IPAMClient{
		typeStr: typeStr,
		client:  client,
	}
}

// IPAMClient implements Interface
type IPAMClient struct {
	typeStr string
	client  versioned.Interface
}

// AutoAssign automatically assigns one or more IP addresses as specified by the
// provided AutoAssignArgs.  AutoAssign returns the list of the assigned IPv4 addresses,
// and the list of the assigned IPv6 addresses.
//
// In case of error, returns the IPs allocated so far along with the error.
func (c IPAMClient) AutoAssign(args AutoAssignArgs) (*current.Result, error) {
	var (
		result current.Result
		err    error
		ip     *cnet.IPNet
		pool   *v1alpha1.IPPool
		block  *v1alpha1.IPAMBlock
	)

	for i := 0; i < datastoreRetries; i++ {
		pool, err = c.client.NetworkV1alpha1().IPPools().Get(context.Background(), args.Pool, metav1.GetOptions{})
		if err != nil {
			return nil, ErrNoQualifiedPool
		}

		if pool.Disabled() {
			klog.Infof("provided ippool %s should be enabled", pool.Name)
			return nil, ErrNoQualifiedPool
		}

		if pool.TypeInvalid() {
			return nil, ErrUnknowIPPoolType
		}

		block, ip, err = c.autoAssign(args.HandleID, args.Attrs, pool)
		if err != nil {
			if errors.Is(err, ErrNoFreeBlocks) {
				return nil, err
			}
			continue
		}
		break
	}

	if err != nil {
		klog.Infof("AutoAssign: args=%s, err=%v", spew.Sdump(args), err)
		return nil, ErrMaxRetry
	}

	args.Info.IPPool = pool.Name
	args.Info.Block = block.Name

	version := 4
	if ip.IP.To4() == nil {
		version = 6
	}

	result.IPs = append(result.IPs, &current.IPConfig{
		Version: fmt.Sprintf("%d", version),
		Address: net.IPNet{IP: ip.IP, Mask: ip.Mask},
		Gateway: net.ParseIP(pool.Spec.Gateway),
	})

	for _, route := range pool.Spec.Routes {
		_, dst, _ := net.ParseCIDR(route.Dst)
		result.Routes = append(result.Routes, &cnitypes.Route{
			Dst: *dst,
			GW:  net.ParseIP(route.GW),
		})
	}
	result.DNS.Domain = pool.Spec.DNS.Domain
	result.DNS.Options = pool.Spec.DNS.Options
	result.DNS.Nameservers = pool.Spec.DNS.Nameservers
	result.DNS.Search = pool.Spec.DNS.Search

	poolType := pool.Spec.Type
	switch poolType {
	case v1alpha1.VLAN:
		result.Interfaces = append(result.Interfaces, &current.Interface{
			Mac: utils.EthRandomAddr(ip.IP),
		})
	}

	return &result, nil
}

//findOrClaimBlock find an address block with free space, and if it doesn't exist, create it.
func (c IPAMClient) findOrClaimBlock(pool *v1alpha1.IPPool, minFreeIps int) (*v1alpha1.IPAMBlock, error) {
	remainingBlocks, err := c.ListBlocks(pool.Name)
	if err != nil {
		return nil, err
	}

	// First, we try to find a block from one of the existing blocks.
	for len(remainingBlocks) > 0 {
		// Pop first cidr.
		block := remainingBlocks[0]
		remainingBlocks = remainingBlocks[1:]

		// Pull out the block.
		if block.NumFreeAddresses() >= minFreeIps {
			return &block, nil
		} else {
			continue
		}
	}

	//Second, create unused Address Blocks
	b, err := c.findUnclaimedBlock(pool)
	if err != nil {
		return nil, err
	}
	blockName := b.BlockName()
	controllerutil.SetControllerReference(pool, b, scheme.Scheme)
	b, err = c.client.NetworkV1alpha1().IPAMBlocks().Create(context.Background(), b, metav1.CreateOptions{})
	if err != nil {
		if k8serrors.IsAlreadyExists(err) {
			b, err = c.queryBlock(blockName)
		}
		if err != nil {
			return nil, err
		}
	}

	if b.NumFreeAddresses() >= minFreeIps {
		return b, nil
	} else {
		errString := fmt.Sprintf("Block '%s' has %d free ips which is less than %d ips required.", b.BlockName(), b.NumFreeAddresses(), minFreeIps)
		return nil, errors.New(errString)
	}
}

func (c IPAMClient) autoAssign(handleID string, attrs map[string]string, requestedPool *v1alpha1.IPPool) (*v1alpha1.IPAMBlock, *cnet.IPNet, error) {
	var (
		block  *v1alpha1.IPAMBlock
		result *cnet.IPNet
		err    error
	)

	block, err = c.findOrClaimBlock(requestedPool, 1)
	if err != nil {
		return nil, nil, err
	}

	result, err = c.autoAssignFromBlock(handleID, attrs, block)
	return block, result, err
}

func (c IPAMClient) autoAssignFromBlock(handleID string, attrs map[string]string, requestedBlock *v1alpha1.IPAMBlock) (*cnet.IPNet, error) {
	var (
		result *cnet.IPNet
		err    error
	)

	for i := 0; i < datastoreRetries; i++ {
		result, err = c.assignFromExistingBlock(requestedBlock, handleID, attrs)
		if err != nil {
			if k8serrors.IsConflict(err) {
				requestedBlock, err = c.queryBlock(requestedBlock.Name)
				if err != nil {
					return nil, err
				}

				// Block b is in sync with datastore. Retry assigning IP.
				continue
			}
			return nil, err
		}
		return result, nil
	}

	return nil, ErrMaxRetry
}

func (c IPAMClient) assignFromExistingBlock(block *v1alpha1.IPAMBlock, handleID string, attrs map[string]string) (*cnet.IPNet, error) {
	ips := block.AutoAssign(1, handleID, attrs)
	if len(ips) == 0 {
		return nil, fmt.Errorf("block %s has no availabe IP", block.BlockName())
	}

	err := c.incrementHandle(handleID, block, 1)
	if err != nil {
		return nil, err
	}

	_, err = c.client.NetworkV1alpha1().IPAMBlocks().Update(context.Background(), block, metav1.UpdateOptions{})
	if err != nil {
		if err := c.decrementHandle(handleID, block, 1); err != nil {
			klog.Errorf("Failed to decrement handle %s", handleID)
		}
		return nil, err
	}

	return &ips[0], nil
}

// ReleaseByHandle releases all IP addresses that have been assigned
// using the provided handle.
func (c IPAMClient) ReleaseByHandle(handleID string) error {
	handle, err := c.queryHandle(handleID)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	for blockStr, _ := range handle.Spec.Block {
		blockName := v1alpha1.ConvertToBlockName(blockStr)
		if err := c.releaseByHandle(handleID, blockName); err != nil {
			return err
		}
	}
	return nil
}

func (c IPAMClient) releaseByHandle(handleID string, blockName string) error {
	for i := 0; i < datastoreRetries; i++ {
		block, err := c.queryBlock(blockName)
		if err != nil {
			if k8serrors.IsNotFound(err) {
				// Block doesn't exist, so all addresses are already
				// unallocated.  This can happen when a handle is
				// overestimating the number of assigned addresses.
				return nil
			} else {
				return err
			}
		}

		num := block.ReleaseByHandle(handleID)
		if num == 0 {
			// Block has no addresses with this handle, so
			// all addresses are already unallocated.
			return nil
		}

		if block.Empty() {
			// TODO: reserve block
			/*
				if err = c.DeleteBlock(block); err != nil {
					if k8serrors.IsConflict(err) {
						// Update conflict - retry.
						continue
					} else if !k8serrors.IsNotFound(err) {
						return err
					}
				}
			*/
			_, err = c.client.NetworkV1alpha1().IPAMBlocks().Update(context.Background(), block, metav1.UpdateOptions{})
			if err != nil {
				if k8serrors.IsConflict(err) {
					// Comparison failed - retry.
					continue
				} else {
					// Something else - return the error.
					return err
				}
			}
		} else {
			// Compare and swap the AllocationBlock using the original
			// KVPair read from before.  No need to update the Value since we
			// have been directly manipulating the value referenced by the KVPair.
			_, err = c.client.NetworkV1alpha1().IPAMBlocks().Update(context.Background(), block, metav1.UpdateOptions{})
			if err != nil {
				if k8serrors.IsConflict(err) {
					// Comparison failed - retry.
					continue
				} else {
					// Something else - return the error.
					return err
				}
			}
		}

		if err = c.decrementHandle(handleID, block, num); err != nil {
			klog.Errorf("Failed to decrement handle %s, err=%s", handleID, err)
		}

		return nil
	}
	return ErrMaxRetry
}

func (c IPAMClient) incrementHandle(handleID string, block *v1alpha1.IPAMBlock, num int) error {
	for i := 0; i < datastoreRetries; i++ {
		create := false
		handle, err := c.queryHandle(handleID)
		if err != nil {
			if k8serrors.IsNotFound(err) {
				// Handle doesn't exist - create it.
				handle = &v1alpha1.IPAMHandle{
					ObjectMeta: v1.ObjectMeta{
						Name: handleID,
					},
					Spec: v1alpha1.IPAMHandleSpec{
						HandleID: handleID,
						Block:    map[string]int{},
					},
				}
				create = true
			} else {
				// Unexpected error reading handle.
				return err
			}
		}

		// Increment the handle for this block.
		handle.IncrementBlock(block, num)

		if create {
			_, err = c.client.NetworkV1alpha1().IPAMHandles().Create(context.Background(), handle, metav1.CreateOptions{})
		} else {
			_, err = c.client.NetworkV1alpha1().IPAMHandles().Update(context.Background(), handle, metav1.UpdateOptions{})
		}
		if err != nil {
			if k8serrors.IsAlreadyExists(err) || k8serrors.IsConflict(err) {
				continue
			}
			return err
		}

		return nil
	}

	return ErrMaxRetry
}

func (c IPAMClient) decrementHandle(handleID string, block *v1alpha1.IPAMBlock, num int) error {
	for i := 0; i < datastoreRetries; i++ {
		handle, err := c.queryHandle(handleID)
		if err != nil {
			return err
		}

		_, err = handle.DecrementBlock(block, num)
		if err != nil {
			klog.Errorf("decrementHandle: %v", err)
			return err
		}

		// Update / Delete as appropriate.  Since we have been manipulating the
		// data in the KVPair, just pass this straight back to the client.
		if handle.Empty() {
			if err = c.deleteHandle(handle); err != nil {
				if k8serrors.IsConflict(err) {
					// Update conflict - retry.
					continue
				} else if !k8serrors.IsNotFound(err) {
					return err
				}
			}
		} else {
			if _, err = c.client.NetworkV1alpha1().IPAMHandles().Update(context.Background(), handle, metav1.UpdateOptions{}); err != nil {
				if k8serrors.IsConflict(err) {
					// Update conflict - retry.
					continue
				}
				return err
			}
		}

		return nil
	}

	return ErrMaxRetry
}

// GetUtilization returns IP utilization info for the specified pools, or for all pools.
func (c IPAMClient) GetUtilization(args GetUtilizationArgs) ([]*PoolUtilization, error) {
	var usage []*PoolUtilization

	// Read all pools.
	allPools, err := c.getAllPools()
	if err != nil {
		return nil, err
	}

	if len(allPools) <= 0 {
		return nil, fmt.Errorf("not found pool")
	}

	// Identify the ones we want and create a PoolUtilization for each of those.
	wantAllPools := len(args.Pools) == 0
	wantedPools := set.FromArray(args.Pools)
	for _, pool := range allPools {
		if wantAllPools || wantedPools.Contains(pool.Name) {
			cap := pool.NumAddresses()
			reserved := pool.NumReservedAddresses()
			usage = append(usage, &PoolUtilization{
				Name:        pool.Name,
				Capacity:    cap,
				Reserved:    reserved,
				Allocate:    0,
				Unallocated: cap - reserved,
			})
		}
	}

	// Find which pool this block belongs to.
	for _, poolUse := range usage {
		blocks, err := c.ListBlocks(poolUse.Name)
		if err != nil {
			return nil, err
		}

		if len(blocks) <= 0 {
			continue
		} else {
			poolUse.Reserved = 0
			poolUse.Allocate = 0
		}

		for _, block := range blocks {
			poolUse.Allocate += block.NumAddresses() - block.NumFreeAddresses() - block.NumReservedAddresses()
			poolUse.Reserved += block.NumReservedAddresses()
		}

		poolUse.Unallocated = poolUse.Capacity - poolUse.Allocate - poolUse.Reserved
	}

	return usage, nil
}

func (c IPAMClient) GetPoolBlocksUtilization(args GetUtilizationArgs) ([]*PoolBlocksUtilization, error) {
	var usage []*PoolBlocksUtilization

	// Read all pools.
	allPools, err := c.getAllPools()
	if err != nil {
		return nil, err
	}

	if len(allPools) <= 0 {
		return nil, fmt.Errorf("not found pool")
	}

	// Identify the ones we want and create a PoolUtilization for each of those.
	wantAllPools := len(args.Pools) == 0
	wantedPools := set.FromArray(args.Pools)
	for _, pool := range allPools {
		if wantAllPools || wantedPools.Contains(pool.Name) {
			cap := pool.NumAddresses()
			reserved := pool.NumReservedAddresses()
			usage = append(usage, &PoolBlocksUtilization{
				Name:        pool.Name,
				Capacity:    cap,
				Reserved:    reserved,
				Allocate:    0,
				Unallocated: cap - reserved,
			})
		}
	}

	// Find which pool this block belongs to.
	for _, poolUse := range usage {
		blocks, err := c.ListBlocks(poolUse.Name)
		if err != nil {
			return nil, err
		}

		if len(blocks) <= 0 {
			continue
		} else {
			poolUse.Reserved = 0
			poolUse.Allocate = 0
		}

		for _, block := range blocks {
			cap := block.NumAddresses()
			free := block.NumFreeAddresses()
			reserved := block.NumReservedAddresses()
			poolUse.Allocate += cap - free - reserved
			poolUse.Reserved += reserved
			poolUse.Blocks = append(poolUse.Blocks, &BlockUtilization{
				Name:        block.Name,
				Capacity:    cap,
				Reserved:    reserved,
				Allocate:    cap - free - reserved,
				Unallocated: free,
			})
		}

		poolUse.Unallocated = poolUse.Capacity - poolUse.Allocate - poolUse.Reserved
	}

	return usage, nil
}

// findUnclaimedBlock finds a block cidr which does not yet exist within the given list of pools. The provided pools
// should already be sanitized and only include existing, enabled pools. Note that the block may become claimed
// between receiving the cidr from this function and attempting to claim the corresponding block as this function
// does not reserve the returned IPNet.
func (c IPAMClient) findUnclaimedBlock(pool *v1alpha1.IPPool) (*v1alpha1.IPAMBlock, error) {
	var result *v1alpha1.IPAMBlock

	// List blocks up front to reduce number of queries.
	// We will try to write the block later to prevent races.
	existingBlocks, err := c.ListBlocks(pool.Name)
	if err != nil {
		return nil, err
	}

	/// Build a map for faster lookups.
	exists := map[string]bool{}
	for _, e := range existingBlocks {
		exists[fmt.Sprintf("%s", e.Spec.CIDR)] = true
	}

	// Iterate through pools to find a new block.
	_, cidr, _ := cnet.ParseCIDR(pool.Spec.CIDR)
	poolType := pool.Spec.Type
	switch poolType {
	case v1alpha1.VLAN:
		if _, ok := exists[cidr.String()]; !ok {
			var reservedAttr *v1alpha1.ReservedAttr
			if pool.Spec.RangeStart != "" && pool.Spec.RangeEnd != "" {
				reservedAttr = &v1alpha1.ReservedAttr{
					StartOfBlock: pool.StartReservedAddressed(),
					EndOfBlock:   pool.EndReservedAddressed(),
					Handle:       v1alpha1.ReservedHandle,
					Note:         v1alpha1.ReservedNote,
				}
			}
			result = v1alpha1.NewBlock(pool, *cidr, reservedAttr)
		}
	default:
		blocks := blockGenerator(pool)
		for subnet := blocks(); subnet != nil; subnet = blocks() {
			// Check if a block already exists for this subnet.
			if _, ok := exists[fmt.Sprintf("%s", subnet.String())]; !ok {
				reservedAttr := &v1alpha1.ReservedAttr{
					StartOfBlock: StartReservedAddressed(*subnet, pool.Spec.RangeStart),
					EndOfBlock:   EndReservedAddressed(*subnet, pool.Spec.RangeEnd),
					Handle:       v1alpha1.ReservedHandle,
					Note:         v1alpha1.ReservedNote,
				}
				result = v1alpha1.NewBlock(pool, *subnet, reservedAttr)
				break
			}
		}
	}

	if result != nil {
		return result, nil
	} else {
		return nil, ErrNoFreeBlocks
	}
}

func (c IPAMClient) ListBlocks(pool string) ([]v1alpha1.IPAMBlock, error) {
	blocks, err := c.client.NetworkV1alpha1().IPAMBlocks().List(context.Background(), metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{
			v1alpha1.IPPoolNameLabel: pool,
		}).String(),
	})
	if err != nil {
		return nil, err
	}

	return blocks.Items, nil
}

// DeleteBlock deletes the given block.
func (c IPAMClient) DeleteBlock(b *v1alpha1.IPAMBlock) error {
	if !b.IsDeleted() {
		b.MarkDeleted()
		_, err := c.client.NetworkV1alpha1().IPAMBlocks().Update(context.Background(), b, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}
	return c.client.NetworkV1alpha1().IPAMBlocks().Delete(context.Background(), b.Name, metav1.DeleteOptions{})
}

func (c IPAMClient) queryBlock(blockName string) (*v1alpha1.IPAMBlock, error) {
	block, err := c.client.NetworkV1alpha1().IPAMBlocks().Get(context.Background(), blockName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	if block.IsDeleted() {
		err := c.DeleteBlock(block)
		if err != nil {
			return nil, err
		}

		return nil, k8serrors.NewNotFound(v1alpha1.Resource(v1alpha1.ResourcePluralIPAMBlock), blockName)
	}

	return block, nil
}

// queryHandle gets a handle for the given handleID key.
func (c IPAMClient) queryHandle(handleID string) (*v1alpha1.IPAMHandle, error) {
	handle, err := c.client.NetworkV1alpha1().IPAMHandles().Get(context.Background(), handleID, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	if handle.IsDeleted() {
		err := c.deleteHandle(handle)
		if err != nil {
			return nil, err
		}

		return nil, k8serrors.NewNotFound(v1alpha1.Resource(v1alpha1.ResourcePluralIPAMHandle), handleID)
	}

	return handle, nil
}

// deleteHandle deletes the given handle.
func (c IPAMClient) deleteHandle(h *v1alpha1.IPAMHandle) error {
	if !h.IsDeleted() {
		h.MarkDeleted()
		_, err := c.client.NetworkV1alpha1().IPAMHandles().Update(context.Background(), h, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}
	return c.client.NetworkV1alpha1().IPAMHandles().Delete(context.Background(), h.Name, metav1.DeleteOptions{})
}

func (c IPAMClient) AutoAssignFromPools(args AutoAssignArgs) (*current.Result, error) {
	utils, err := c.GetUtilization(GetUtilizationArgs{args.Pools})
	if err != nil {
		return nil, err
	}

	for _, util := range utils {
		if util.Unallocated > 1 {
			args.Pool = util.Name
			if r, err := c.AutoAssign(args); err != nil {
				klog.Warningf("AutoAssign from pool %s failed: %v", util.Name, err)
				continue
			} else {
				return r, nil
			}
		}
	}

	return nil, fmt.Errorf("no appropriate ippool found")
}

func (c IPAMClient) AutoAssignFromBlocks(args AutoAssignArgs) (*current.Result, error) {
	var blocks []*v1alpha1.IPAMBlock
	for _, block := range args.Blocks {
		if b, err := c.client.NetworkV1alpha1().IPAMBlocks().Get(context.Background(), block, v1.GetOptions{}); err == nil {
			blocks = append(blocks, b)
		} else {
			klog.Warningf("Get block %s failed: %v", block, err)
		}
	}

	for _, block := range blocks {
		if block.NumFreeAddresses() >= 1 {
			if ip, err := c.autoAssignFromBlock(args.HandleID, args.Attrs, block); err == nil {
				poolName := block.Labels[networkv1alpha1.IPPoolNameLabel]
				if pool, err := c.client.NetworkV1alpha1().IPPools().Get(context.Background(), poolName, v1.GetOptions{}); err == nil {
					args.Info.IPPool = poolName
					args.Info.Block = block.Name
					return IP2Resutl(ip, pool), nil
				} else {
					klog.Errorf("autoAssignFromBlock %s failed to find ippool %s: %v", block.Name, poolName, err)
				}
			} else {
				klog.Warningf("autoAssignFromBlock %s failed: %v", block.Name, err)
			}
		} else {
			continue
		}
	}

	return nil, ErrMaxRetry
}

func (c IPAMClient) AutoGenerateBlocksFromPool(poolName string) error {
	pool, err := c.client.NetworkV1alpha1().IPPools().Get(context.Background(), poolName, metav1.GetOptions{})
	if err != nil {
		return ErrNoQualifiedPool
	}

	// List blocks up front to reduce number of queries.
	// We will try to write the block later to prevent races.
	existingBlocks, err := c.ListBlocks(pool.Name)
	if err != nil {
		return err
	}

	/// Build a map for faster lookups.
	exists := map[string]bool{}
	for _, e := range existingBlocks {
		exists[fmt.Sprintf("%s", e.Spec.CIDR)] = true
	}

	blocks := blockGenerator(pool)
	for subnet := blocks(); subnet != nil; subnet = blocks() {
		// Check if a block already exists for this subnet.
		if _, ok := exists[fmt.Sprintf("%s", subnet.String())]; !ok {
			reservedAttr := &v1alpha1.ReservedAttr{
				StartOfBlock: StartReservedAddressed(*subnet, pool.Spec.RangeStart),
				EndOfBlock:   EndReservedAddressed(*subnet, pool.Spec.RangeEnd),
				Handle:       v1alpha1.ReservedHandle,
				Note:         v1alpha1.ReservedNote,
			}
			block := v1alpha1.NewBlock(pool, *subnet, reservedAttr)
			blockName := block.BlockName()
			controllerutil.SetControllerReference(pool, block, scheme.Scheme)
			block, err = c.client.NetworkV1alpha1().IPAMBlocks().Create(context.Background(), block, metav1.CreateOptions{})
			if err != nil {
				if k8serrors.IsAlreadyExists(err) {
					block, err = c.queryBlock(blockName)
				}
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// Generator to get list of block CIDRs which
// fall within the given cidr. The passed in pool
// must contain the passed in block cidr.
// Returns nil when no more blocks can be generated.
func blockGenerator(pool *v1alpha1.IPPool) func() *cnet.IPNet {
	tmp, cidr, _ := cnet.ParseCIDR(pool.Spec.CIDR)
	ip := *tmp

	var blockMask net.IPMask
	if ip.Version() == 4 {
		blockMask = net.CIDRMask(pool.Spec.BlockSize, 32)
	} else {
		blockMask = net.CIDRMask(pool.Spec.BlockSize, 128)
	}

	ones, size := blockMask.Size()
	blockSize := new(big.Int).Exp(big.NewInt(2), big.NewInt(int64(size-ones)), nil)

	return func() *cnet.IPNet {
		returnIP := ip

		if cidr.Contains(ip.IP) {
			ipnet := net.IPNet{IP: returnIP.IP, Mask: blockMask}
			cidr := cnet.IPNet{IPNet: ipnet}
			ip = cnet.IncrementIP(ip, blockSize)
			return &cidr
		} else {
			return nil
		}
	}
}

func StartReservedAddressed(cidr cnet.IPNet, r string) int {
	if r == "" {
		return 0
	}

	ip := *cnet.ParseIP(r)
	ipAsInt := cnet.IPToBigInt(ip)
	baseInt := cnet.IPToBigInt(cnet.IP{IP: cidr.IP})
	ord := big.NewInt(0).Sub(ipAsInt, baseInt).Int64()

	ones, size := cidr.Mask.Size()
	numAddresses := 1 << uint(size-ones)

	if ord < 0 || ord >= int64(numAddresses) {
		// TODO: handle error
		return 0
	}
	return int(ord)
}

func EndReservedAddressed(cidr cnet.IPNet, r string) int {
	ones, size := cidr.Mask.Size()
	total := 1 << uint(size-ones)
	if r == "" {
		return 0
	}

	ip := *cnet.ParseIP(r)
	ipAsInt := cnet.IPToBigInt(ip)
	baseInt := cnet.IPToBigInt(cnet.IP{IP: cidr.IP})
	ord := big.NewInt(0).Sub(ipAsInt, baseInt).Int64()
	if ord < 0 || ord >= int64(total) {
		// TODO: handle error
		return 0
	}

	return total - int(ord) - 1
}

func IP2Resutl(ip *cnet.IPNet, pool *v1alpha1.IPPool) *current.Result {
	var result current.Result

	version := 4
	if ip.IP.To4() == nil {
		version = 6
	}

	result.IPs = append(result.IPs, &current.IPConfig{
		Version: fmt.Sprintf("%d", version),
		Address: net.IPNet{IP: ip.IP, Mask: ip.Mask},
		Gateway: net.ParseIP(pool.Spec.Gateway),
	})

	for _, route := range pool.Spec.Routes {
		_, dst, _ := net.ParseCIDR(route.Dst)
		result.Routes = append(result.Routes, &cnitypes.Route{
			Dst: *dst,
			GW:  net.ParseIP(route.GW),
		})
	}
	result.DNS.Domain = pool.Spec.DNS.Domain
	result.DNS.Options = pool.Spec.DNS.Options
	result.DNS.Nameservers = pool.Spec.DNS.Nameservers
	result.DNS.Search = pool.Spec.DNS.Search

	poolType := pool.Spec.Type
	switch poolType {
	case v1alpha1.VLAN:
		result.Interfaces = append(result.Interfaces, &current.Interface{
			Mac: utils.EthRandomAddr(ip.IP),
		})
	}

	return &result
}

// AssignFixIps fix ip form assign pool or block
func (c IPAMClient) AssignFixIps(handleID string, ipList, pools, blocks []string, info *PoolInfo) (*current.Result, error) {
	fixArgs := FixIpArgs{
		AutoAssignArgs: AutoAssignArgs{
			HandleID: handleID,
			Pools:    pools,
			Blocks:   blocks,
			Info:     info,
		},
		TarGetIpList: ipList,
	}
	if len(fixArgs.Pools) > 0 {
		return c.FixIpsFromPool(fixArgs)
	} else if len(fixArgs.Blocks) > 0 {
		return c.FixIpsFromBlock(fixArgs)
	}
	return nil, fmt.Errorf("no suitable pool and block")
}

// FixIpsFromPool fix ip from assign pool
func (c IPAMClient) FixIpsFromPool(args FixIpArgs) (*current.Result, error) {
	for i := 0; i < len(args.Pools); i++ {
		args.Pool = args.Pools[i]
		blocks, err := c.ListBlocks(args.Pool)
		if err != nil {
			return nil, fmt.Errorf("%s get listBlock err: %v", args.Pool, err)
		}

		for j := 0; j < len(blocks); j++ {
			block := blocks[j]
			if block.NumFreeAddresses() < 1 {
				continue
			}
			args.Pool = block.Labels[networkv1alpha1.IPPoolNameLabel]
			if result, err := c.retryFixIP(&block, args); err == nil {
				args.Info.IPPool = args.Pool
				args.Info.Block = block.Name
				return result, nil
			} else {
				klog.Warningf("FixIpsFromPool for %s from %s failed: %v", args.TarGetIpList, block.Name, err)
			}
		}
	}

	return nil, fmt.Errorf("fixIP for %s from pool %s failed", args.TarGetIpList, args.Pools)
}

// FixIpsFromBlock fix ip form assign block
func (c IPAMClient) FixIpsFromBlock(args FixIpArgs) (*current.Result, error) {
	var blocks []*v1alpha1.IPAMBlock
	for _, block := range args.Blocks {
		if b, err := c.client.NetworkV1alpha1().IPAMBlocks().Get(context.Background(), block, v1.GetOptions{}); err == nil {
			blocks = append(blocks, b)
		} else {
			klog.Warningf("Get block %s failed: %v", block, err)
		}
	}

	for i := 0; i < len(blocks); i++ {
		block := blocks[i]
		args.Pool = block.Labels[networkv1alpha1.IPPoolNameLabel]
		if block.NumFreeAddresses() > 0 {
			if result, err := c.retryFixIP(block, args); err == nil {
				args.Info.IPPool = args.Pool
				args.Info.Block = block.Name
				return result, nil
			} else {
				klog.Warningf("FixIpsFromBlock for %s from %s failed: %v", args.TarGetIpList, block.Name, err)
			}
		}
	}

	return nil, fmt.Errorf("fixIP for %s from block %s failed", args.TarGetIpList, args.Blocks)
}

func (c IPAMClient) retryFixIP(requestBlock *networkv1alpha1.IPAMBlock, args FixIpArgs) (*current.Result, error) {
	var result *current.Result
	for i := 0; i < datastoreRetries; i++ {
		pool, err := c.client.NetworkV1alpha1().IPPools().Get(context.Background(), args.Pool, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get pool err: %v", err)
		}
		if pool.Disabled() {
			return nil, fmt.Errorf("get pool err: %v", ErrNoQualifiedPool)
		}
		if pool.TypeInvalid() {
			return nil, fmt.Errorf("get pool err: %v", ErrUnknowIPPoolType)
		}

		ip, err := c.fixIP(requestBlock, args.HandleID, args.TarGetIpList, args.Attrs)
		if err != nil {
			if k8serrors.IsConflict(err) {
				requestBlock, err = c.queryBlock(requestBlock.Name)
				if err != nil {
					return nil, err
				}
				continue
			}
			return nil, err
		}

		result = IP2Resutl(&cnet.IPNet{
			IPNet: net.IPNet{IP: ip},
		}, pool)
		return result, nil
	}
	return nil, ErrMaxRetry
}

// fixIP check annotation ip is unallocated
func (c IPAMClient) fixIP(block *v1alpha1.IPAMBlock, handleID string, ipList []string, attrs map[string]string) (net.IP, error) {
	_, cidr, err := cnet.ParseCIDR(block.Spec.CIDR)
	if err != nil {
		return nil, fmt.Errorf("parse cidr err: %v", err)
	}

	for _, tarIp := range ipList {
		ip := cnet.ParseIP(tarIp)
		if ip != nil && !cidr.Contains(ip.IP) {
			continue
		}
		ordinal, err := block.IPToOrdinal(*ip)
		if err != nil {
			continue
		}

		for j, unUsedIp := range block.Spec.Unallocated {
			if ordinal == unUsedIp {
				block.Spec.Unallocated = append(block.Spec.Unallocated[:j], block.Spec.Unallocated[j+1:]...)
				attribute := updateAttribute(block, handleID, attrs)
				block.Spec.Allocations[ordinal] = &attribute

				err = c.incrementHandle(handleID, block, 1)
				if err != nil {
					return nil, err
				}
				_, err = c.client.NetworkV1alpha1().IPAMBlocks().Update(context.Background(), block, metav1.UpdateOptions{})
				if err != nil {
					if err := c.decrementHandle(handleID, block, 1); err != nil {
						klog.Errorf("Failed to decrement handle %s", handleID)
					}
					return nil, err
				}

				return ip.IP, nil
			}
		}
	}
	return nil, fmt.Errorf("not free ip in this block: %s", block.Name)
}

func updateAttribute(b *networkv1alpha1.IPAMBlock, handleID string, attrs map[string]string) int {
	attr := networkv1alpha1.AllocationAttribute{AttrPrimary: handleID, AttrSecondary: attrs}
	for idx, existing := range b.Spec.Attributes {
		if reflect.DeepEqual(attr, existing) {
			return idx
		}
	}

	// Does not exist - add it.
	attrIndex := len(b.Spec.Attributes)
	b.Spec.Attributes = append(b.Spec.Attributes, attr)
	return attrIndex
}
