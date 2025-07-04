// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package endpoint

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	"github.com/cilium/cilium/api/v1/models"
	"github.com/cilium/cilium/pkg/common"
	"github.com/cilium/cilium/pkg/controller"
	"github.com/cilium/cilium/pkg/datapath/linux/safenetlink"
	"github.com/cilium/cilium/pkg/endpoint/regeneration"
	"github.com/cilium/cilium/pkg/fqdn"
	"github.com/cilium/cilium/pkg/fqdn/restore"
	"github.com/cilium/cilium/pkg/identity"
	"github.com/cilium/cilium/pkg/labels"
	"github.com/cilium/cilium/pkg/labelsfilter"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/mac"
	"github.com/cilium/cilium/pkg/node"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/time"
)

var (
	restoreEndpointIdentityControllerGroup = controller.NewGroup("restore-endpoint-identity")
	initialGlobalIdentitiesControllerGroup = controller.NewGroup("initial-global-identities")
)

type EndpointParser interface {
	ParseEndpoint(epJSON []byte) (*Endpoint, error)
}

// ReadEPsFromDirNames returns a mapping of endpoint ID to endpoint of endpoints
// from a list of directory names that can possible contain an endpoint.
func ReadEPsFromDirNames(ctx context.Context, logger *slog.Logger, parser EndpointParser, basePath string, eptsDirNames []string) map[uint16]*Endpoint {
	completeEPDirNames, incompleteEPDirNames := partitionEPDirNamesByRestoreStatus(eptsDirNames)

	if len(incompleteEPDirNames) > 0 {
		for _, epDirName := range incompleteEPDirNames {
			fullDirName := filepath.Join(basePath, epDirName)
			logger.Info(
				fmt.Sprintf("Found incomplete restore directory %s. Removing it...", fullDirName),
				logfields.EndpointID, epDirName,
			)
			if err := os.RemoveAll(epDirName); err != nil {
				logger.Warn(
					fmt.Sprintf("Error while removing directory %s. Ignoring it...", fullDirName),
					logfields.Error, err,
					logfields.EndpointID, epDirName,
				)
			}
		}
	}

	possibleEPs := map[uint16]*Endpoint{}
	for _, epDirName := range completeEPDirNames {
		epDir := filepath.Join(basePath, epDirName)

		scopedLogger := logger.With(
			logfields.EndpointID, epDirName,
			logfields.Path, epDir,
		)

		state, err := findEndpointState(scopedLogger, epDir)
		if err != nil {
			scopedLogger.Warn("Couldn't find state, ignoring endpoint", logfields.Error, err)
			continue
		}

		ep, err := parser.ParseEndpoint(state)
		if err != nil {
			scopedLogger.Warn("Unable to parse the C header file", logfields.Error, err)
			continue
		}
		if _, ok := possibleEPs[ep.ID]; ok {
			// If the endpoint already exists then give priority to the directory
			// that contains an endpoint that didn't fail to be build.
			if strings.HasSuffix(ep.DirectoryPath(), epDirName) {
				possibleEPs[ep.ID] = ep
			}
		} else {
			possibleEPs[ep.ID] = ep
		}

		// We need to save the host endpoint ID as we'll need it to regenerate
		// other endpoints.
		if ep.IsHost() {
			node.SetEndpointID(ep.GetID())
		}
	}
	return possibleEPs
}

// findEndpointState finds the JSON representation of an endpoint's state in
// a directory.
//
// It prefers reading from the endpoint state JSON file and falls back to
// reading from the header.
func findEndpointState(logger *slog.Logger, dir string) ([]byte, error) {
	state, err := os.ReadFile(filepath.Join(dir, common.EndpointStateFileName))
	if err == nil {
		logger.Debug("Restore from JSON file")
		return state, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	// Fall back to reading state from the C header.
	// Remove this at some point in the far future.
	f, err := os.Open(filepath.Join(dir, common.CHeaderFileName))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	logger.Debug("Restore from C header file")

	br := bufio.NewReader(f)
	var line []byte
	for {
		b, err := br.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return nil, os.ErrNotExist
		}
		if err != nil {
			return nil, err
		}
		if bytes.Contains(b, []byte(ciliumCHeaderPrefix)) {
			line = b
			break
		}
	}

	epSlice := bytes.Split(line, []byte{':'})
	if len(epSlice) != 2 {
		return nil, fmt.Errorf("invalid format %q. Should contain a single ':'", line)
	}

	return base64.StdEncoding.AppendDecode(nil, epSlice[1])
}

// partitionEPDirNamesByRestoreStatus partitions the provided list of directory
// names that can possibly contain an endpoint, into two lists, containing those
// names that represent an incomplete endpoint restore and those that do not.
func partitionEPDirNamesByRestoreStatus(eptsDirNames []string) (complete []string, incomplete []string) {
	dirNames := make(map[string]struct{}, len(eptsDirNames))
	for _, epDirName := range eptsDirNames {
		dirNames[epDirName] = struct{}{}
	}

	incompleteSuffixes := []string{nextDirectorySuffix, nextFailedDirectorySuffix}
	incompleteSet := make(map[string]struct{})

	for _, epDirName := range eptsDirNames {
		for _, suff := range incompleteSuffixes {
			if strings.HasSuffix(epDirName, suff) {
				if _, exists := dirNames[epDirName[:len(epDirName)-len(suff)]]; exists {
					incompleteSet[epDirName] = struct{}{}
				}
			}
		}
	}

	for epDirName := range dirNames {
		if _, exists := incompleteSet[epDirName]; exists {
			incomplete = append(incomplete, epDirName)
		} else {
			complete = append(complete, epDirName)
		}
	}

	return
}

// RegenerateAfterRestore performs the following operations on the specified
// Endpoint:
// * allocates an identity for the Endpoint
// * fetches the latest labels from the pod.
// * regenerates the endpoint
// Returns an error if any operation fails while trying to perform the above
// operations.
func (e *Endpoint) RegenerateAfterRestore(regenerator *Regenerator, resolveMetadata MetadataResolverCB) error {
	if err := e.restoreHostIfindex(); err != nil {
		return err
	}

	if err := e.restoreIdentity(regenerator); err != nil {
		return err
	}

	// Now that we have restored the endpoints' identity, run the metadata
	// resolver so that we can fetch the latest labels from the pod for this
	// endpoint.
	e.RunRestoredMetadataResolver(resolveMetadata)

	regenerationMetadata := &regeneration.ExternalRegenerationMetadata{
		Reason:            "syncing state to host",
		RegenerationLevel: regeneration.RegenerateWithDatapath,
	}
	if buildSuccess := <-e.Regenerate(regenerationMetadata); !buildSuccess {
		return fmt.Errorf("failed while regenerating endpoint")
	}

	e.getLogger().Info(
		"Restored endpoint",
		logfields.IPAddrs,
		[]any{
			logfields.IPv4, e.GetIPv4Address(),
			logfields.IPv6, e.GetIPv6Address(),
		},
	)
	return nil
}

// restoreHostIfindex looks up the host interface's ifindex using netlink and
// populates the value in the Endpoint. This used to be left at zero for
// whatever reason, so the zero ifindex got persisted to disk.
//
// Try to populate the ifindex field using netlink so we can rely on it to
// generate the host's endpoint configuration.
func (e *Endpoint) restoreHostIfindex() error {
	if !e.isHost || e.ifIndex != 0 {
		return nil
	}

	l, err := safenetlink.LinkByName(e.ifName)
	if err != nil {
		return fmt.Errorf("get host interface: %w", err)
	}
	e.ifIndex = l.Attrs().Index

	return nil
}

func (e *Endpoint) restoreIdentity(regenerator *Regenerator) error {
	if err := e.rlockAlive(); err != nil {
		e.logDisconnectedMutexAction(err, "before filtering labels during regenerating restored endpoint")
		return err
	}
	// Filter the restored labels with the new daemon's filter
	l, _ := labelsfilter.Filter(e.labels.AllLabels())
	e.runlock()

	// Getting the ep's identity while we are restoring should block the
	// restoring of the endpoint until we get its security identity ID.
	// If the endpoint is removed, this controller will cancel the allocator
	// requests.
	controllerName := fmt.Sprintf("restoring-ep-identity (%v)", e.ID)
	var (
		id                *identity.Identity
		allocatedIdentity = make(chan struct{})
	)
	e.UpdateController(controllerName,
		controller.ControllerParams{
			Group: restoreEndpointIdentityControllerGroup,
			DoFunc: func(ctx context.Context) (err error) {
				id, _, err = e.allocator.AllocateIdentity(ctx, l, true, identity.InvalidIdentity)
				if err != nil {
					return err
				}
				close(allocatedIdentity)
				return nil
			},
		})

	// Wait until we either get an identity or the endpoint is removed or
	// deleted from the node.
	select {
	case <-e.aliveCtx.Done():
		return ErrNotAlive
	case <-allocatedIdentity:
	}

	// Wait for initial identities and ipcache from the
	// kvstore before doing any policy calculation for
	// endpoints that don't have a fixed identity or are
	// not well known.
	if !id.IsFixed() && !id.IsWellKnown() {
		// Getting the initial global identities while we are restoring should
		// block the restoring of the endpoint.
		// If the endpoint is removed, this controller will cancel the allocator
		// WaitForInitialGlobalIdentities function.
		controllerName := fmt.Sprintf("waiting-initial-global-identities-ep (%v)", e.ID)
		gotInitialGlobalIdentities := make(chan struct{})
		e.UpdateController(controllerName,
			controller.ControllerParams{
				Group: initialGlobalIdentitiesControllerGroup,
				DoFunc: func(ctx context.Context) (err error) {
					err = e.allocator.WaitForInitialGlobalIdentities(ctx)
					if err != nil {
						e.getLogger().Warn("Failed while waiting for initial global identities", logfields.Error, err)
						return err
					}
					close(gotInitialGlobalIdentities)
					return nil
				},
			})

		// Wait until we either the initial global identities or the endpoint
		// is deleted.
		select {
		case <-e.aliveCtx.Done():
			return ErrNotAlive
		case <-gotInitialGlobalIdentities:
		}
	}

	// Wait for registered initializers to complete before allowing endpoint regeneration.
	if err := regenerator.WaitForFence(e.aliveCtx); err != nil {
		return err
	}

	if err := e.lockAlive(); err != nil {
		e.getLogger().Warn("Endpoint to restore has been deleted")
		return err
	}

	e.setState(StateRestoring, "Synchronizing endpoint labels with KVStore")

	if e.SecurityIdentity != nil {
		if oldSecID := e.SecurityIdentity.ID; id.ID != oldSecID {
			e.getLogger().Info(
				"Security identity for endpoint is different from the security identity restored for the endpoint",
				logfields.IdentityOld, oldSecID,
				logfields.IdentityNew, id.ID,
			)

			// The identity of the endpoint being
			// restored has changed. This can be
			// caused by two main reasons:
			//
			// 1) Cilium has been upgraded,
			// downgraded or the configuration has
			// changed and the new version or
			// configuration causes different
			// labels to be considered security
			// relevant for this endpoint.
			//
			// Immediately using the identity may
			// cause connectivity problems if this
			// is the first endpoint in the cluster
			// to use the new identity. All other
			// nodes will not have had a chance to
			// adjust the security policies for
			// their endpoints. Hence, apply a
			// grace period to allow for the
			// update. It is not required to check
			// any local endpoints for potential
			// outdated security rules, the
			// notification of the new security
			// identity will have been received and
			// will trigger the necessary
			// recalculation of all local
			// endpoints.
			//
			// 2) The identity is outdated as the
			// state in the kvstore has changed.
			// This reason would justify an
			// immediate use of the new identity
			// but given the current identity is
			// already in place, it is also correct
			// to continue using it for the
			// duration of a grace period.
			time.Sleep(option.Config.IdentityChangeGracePeriod)
		}
	}
	// The identity of a freshly restored endpoint is incomplete due to some
	// parts of the identity not being marshaled to JSON. Hence we must set
	// the identity even if has not changed.
	e.SetIdentity(id, true)
	e.unlock()

	return nil
}

// toSerializedEndpoint converts the Endpoint to its corresponding
// serializableEndpoint, which contains all of the fields that are needed upon
// restoring an Endpoint after cilium-agent restarts.
func (e *Endpoint) toSerializedEndpoint() *serializableEndpoint {
	return &serializableEndpoint{
		ID:                       e.ID,
		ContainerName:            e.GetContainerName(),
		ContainerID:              e.GetContainerID(),
		DockerNetworkID:          e.dockerNetworkID,
		DockerEndpointID:         e.dockerEndpointID,
		IfName:                   e.ifName,
		IfIndex:                  e.ifIndex,
		ParentIfIndex:            e.parentIfIndex,
		ContainerIfName:          e.containerIfName,
		DisableLegacyIdentifiers: e.disableLegacyIdentifiers,
		Labels:                   e.labels,
		LXCMAC:                   e.mac,
		IPv6:                     e.IPv6,
		IPv6IPAMPool:             e.IPv6IPAMPool,
		IPv4:                     e.IPv4,
		IPv4IPAMPool:             e.IPv4IPAMPool,
		NodeMAC:                  e.nodeMAC,
		SecurityIdentity:         e.SecurityIdentity,
		Options:                  e.Options,
		DNSRules:                 e.DNSRules,
		DNSRulesV2:               e.DNSRulesV2,
		DNSHistory:               e.DNSHistory,
		DNSZombies:               e.DNSZombies,
		K8sPodName:               e.K8sPodName,
		K8sNamespace:             e.K8sNamespace,
		K8sUID:                   e.K8sUID,
		DatapathConfiguration:    e.DatapathConfiguration,
		CiliumEndpointUID:        e.ciliumEndpointUID,
		Properties:               e.properties,
		NetnsCookie:              e.NetNsCookie,
	}
}

// serializableEndpoint contains the fields from an Endpoint which are needed to be
// restored if cilium-agent restarts.
//
// WARNING - STABLE API
// This structure is written as JSON to StateDir/{ID}/ep_config.h to allow to
// restore endpoints when the agent is being restarted. The restore operation
// will read the file and re-create all endpoints with all fields which are not
// marked as private to JSON marshal. Do NOT modify this structure in ways which
// is not JSON forward compatible.
type serializableEndpoint struct {
	// ID of the endpoint, unique in the scope of the node
	ID uint16

	// containerName is the name given to the endpoint by the container runtime
	ContainerName string

	// containerID is the container ID that docker has assigned to the endpoint
	// Note: The JSON tag was kept for backward compatibility.
	ContainerID string `json:"dockerID,omitempty"`

	// dockerNetworkID is the network ID of the libnetwork network if the
	// endpoint is a docker managed container which uses libnetwork
	DockerNetworkID string

	// dockerEndpointID is the Docker network endpoint ID if managed by
	// libnetwork
	DockerEndpointID string

	// ifName is the name of the host facing interface (veth pair) which
	// connects into the endpoint
	IfName string

	// ifIndex is the interface index of the host face interface (veth pair)
	IfIndex int

	// parentIfIndex is the interface index of the interface with which the endpoint
	// IP is associated. In some scenarios, the network will expect traffic with
	// the endpoint IP to be sent via the parent interface.
	ParentIfIndex int

	// ContainerIfName is the name of the container facing interface (veth pair).
	ContainerIfName string

	// DisableLegacyIdentifiers disables lookup using legacy endpoint identifiers
	// (container name, container id, pod name) for this endpoint.
	DisableLegacyIdentifiers bool

	// Labels is the endpoint's label configuration
	Labels labels.OpLabels `json:"OpLabels"`

	// mac is the MAC address of the endpoint
	//
	// FIXME: Rename this field to MAC
	LXCMAC mac.MAC // Container MAC address.

	// IPv6 is the IPv6 address of the endpoint
	IPv6 netip.Addr

	// IPv6IPAMPool is the IPAM address pool from which the IPv6 address was allocated
	IPv6IPAMPool string

	// IPv4 is the IPv4 address of the endpoint
	IPv4 netip.Addr

	// IPv4IPAMPool is the IPAM address pool from which the IPv4 address was allocated
	IPv4IPAMPool string

	// nodeMAC is the MAC of the node (agent). The MAC is different for every endpoint.
	NodeMAC mac.MAC

	// SecurityIdentity is the security identity of this endpoint. This is computed from
	// the endpoint's labels.
	SecurityIdentity *identity.Identity `json:"SecLabel"`

	// Options determine the datapath configuration of the endpoint.
	Options *option.IntOptions

	// DNSRules is the collection of current DNS rules for this endpoint.
	DNSRules restore.DNSRules

	// DNSRulesV2 is the collection of current DNS rules for this endpoint,
	// that conform to using V2 of the PortProto key.
	DNSRulesV2 restore.DNSRules

	// DNSHistory is the collection of still-valid DNS responses intercepted for
	// this endpoint.
	DNSHistory *fqdn.DNSCache

	// DNSZombies is the collection of DNS entries that have been expired or
	// evicted from DNSHistory.
	DNSZombies *fqdn.DNSZombieMappings

	// K8sPodName is the Kubernetes pod name of the endpoint
	K8sPodName string

	// K8sNamespace is the Kubernetes namespace of the endpoint
	K8sNamespace string

	// K8sUID is the Kubernetes pod UID of the endpoint
	K8sUID string

	// DatapathConfiguration is the endpoint's datapath configuration as
	// passed in via the plugin that created the endpoint, e.g. the CNI
	// plugin which performed the plumbing will enable certain datapath
	// features according to the mode selected.
	DatapathConfiguration models.EndpointDatapathConfiguration

	// CiliumEndpointUID contains the unique identifier ref for the CiliumEndpoint
	// that this Endpoint was managing.
	// This is used to avoid overwriting/deleting ciliumendpoints that are managed
	// by other endpoints.
	CiliumEndpointUID types.UID

	// Properties are used to store some internal property about this Endpoint.
	Properties map[string]any

	// NetnsCookie is the network namespace cookie of the Endpoint.
	NetnsCookie uint64
}

// UnmarshalJSON expects that the contents of `raw` are a serializableEndpoint,
// which is then converted into an Endpoint.
func (ep *Endpoint) UnmarshalJSON(raw []byte) error {
	// We may have to populate structures in the Endpoint manually to do the
	// translation from serializableEndpoint --> Endpoint.
	restoredEp := &serializableEndpoint{
		Labels:     labels.NewOpLabels(),
		Options:    option.NewIntOptions(&EndpointMutableOptionLibrary),
		DNSHistory: fqdn.NewDNSCacheWithLimit(option.Config.ToFQDNsMinTTL, option.Config.ToFQDNsMaxIPsPerHost),
		DNSZombies: fqdn.NewDNSZombieMappings(ep.getLogger(), option.Config.ToFQDNsMaxDeferredConnectionDeletes, option.Config.ToFQDNsMaxIPsPerHost),
	}
	if err := json.Unmarshal(raw, restoredEp); err != nil {
		return fmt.Errorf("error unmarshaling serializableEndpoint from base64 representation: %w", err)
	}

	ep.fromSerializedEndpoint(restoredEp)
	return nil
}

// MarshalJSON marshals the Endpoint as its serializableEndpoint representation.
func (ep *Endpoint) MarshalJSON() ([]byte, error) {
	return json.Marshal(ep.toSerializedEndpoint())
}

func (ep *Endpoint) fromSerializedEndpoint(r *serializableEndpoint) {
	ep.ID = r.ID
	ep.createdAt = time.Now()
	ep.InitialEnvoyPolicyComputed = make(chan struct{})
	ep.containerName.Store(&r.ContainerName)
	ep.containerID.Store(&r.ContainerID)
	ep.dockerNetworkID = r.DockerNetworkID
	ep.dockerEndpointID = r.DockerEndpointID
	ep.ifName = r.IfName
	ep.ifIndex = r.IfIndex
	ep.parentIfIndex = r.ParentIfIndex
	ep.containerIfName = r.ContainerIfName
	ep.disableLegacyIdentifiers = r.DisableLegacyIdentifiers
	ep.labels = r.Labels
	ep.mac = r.LXCMAC
	ep.IPv6 = r.IPv6
	ep.IPv6IPAMPool = r.IPv6IPAMPool
	ep.IPv4 = r.IPv4
	ep.IPv4IPAMPool = r.IPv4IPAMPool
	ep.nodeMAC = r.NodeMAC
	ep.SecurityIdentity = r.SecurityIdentity
	ep.DNSRules = r.DNSRules
	ep.DNSRulesV2 = r.DNSRulesV2
	ep.DNSHistory = r.DNSHistory
	ep.DNSZombies = r.DNSZombies
	ep.K8sPodName = r.K8sPodName
	ep.K8sNamespace = r.K8sNamespace
	ep.K8sUID = r.K8sUID
	ep.DatapathConfiguration = r.DatapathConfiguration
	ep.Options = r.Options
	ep.ciliumEndpointUID = r.CiliumEndpointUID
	if r.Properties != nil {
		ep.properties = r.Properties
	} else {
		ep.properties = map[string]any{}
	}
	ep.NetNsCookie = r.NetnsCookie
}
