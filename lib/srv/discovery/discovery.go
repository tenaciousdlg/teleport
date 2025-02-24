/*
Copyright 2022 Gravitational, Inc.

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

package discovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v3"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/sirupsen/logrus"
	"golang.org/x/exp/slices"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/client/proto"
	usageeventsv1 "github.com/gravitational/teleport/api/gen/proto/go/usageevents/v1"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/api/types/discoveryconfig"
	apievents "github.com/gravitational/teleport/api/types/events"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/cloud"
	"github.com/gravitational/teleport/lib/cloud/gcp"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/srv/discovery/common"
	"github.com/gravitational/teleport/lib/srv/discovery/fetchers"
	"github.com/gravitational/teleport/lib/srv/discovery/fetchers/db"
	"github.com/gravitational/teleport/lib/srv/server"
)

var errNoInstances = errors.New("all fetched nodes already enrolled")

// Matchers contains all matchers used by discovery service
type Matchers struct {
	// AWS is a list of AWS EC2 matchers.
	AWS []types.AWSMatcher
	// Azure is a list of Azure matchers to discover resources.
	Azure []types.AzureMatcher
	// GCP is a list of GCP matchers to discover resources.
	GCP []types.GCPMatcher
	// Kubernetes is a list of Kubernetes matchers to discovery resources.
	Kubernetes []types.KubernetesMatcher
}

func (m Matchers) IsEmpty() bool {
	return len(m.GCP) == 0 && len(m.AWS) == 0 && len(m.Azure) == 0 && len(m.Kubernetes) == 0
}

// ssmInstaller handles running SSM commands that install Teleport on EC2 instances.
type ssmInstaller interface {
	Run(ctx context.Context, req server.SSMRunRequest) error
}

// azureInstaller handles running commands that install Teleport on Azure
// virtual machines.
type azureInstaller interface {
	Run(ctx context.Context, req server.AzureRunRequest) error
}

// gcpInstaller handles running commands that install Teleport on GCP
// virtual machines.
type gcpInstaller interface {
	Run(ctx context.Context, req server.GCPRunRequest) error
}

// Config provides configuration for the discovery server.
type Config struct {
	// CloudClients is an interface for retrieving cloud clients.
	CloudClients cloud.Clients
	// KubernetesClient is the Kubernetes client interface
	KubernetesClient kubernetes.Interface
	// Matchers stores all types of matchers to discover resources
	Matchers Matchers
	// Emitter is events emitter, used to submit discrete events
	Emitter apievents.Emitter
	// AccessPoint is a discovery access point
	AccessPoint auth.DiscoveryAccessPoint
	// Log is the logger.
	Log logrus.FieldLogger
	// onDatabaseReconcile is called after each database resource reconciliation.
	onDatabaseReconcile func()
	// protocolChecker is used by Kubernetes fetchers to check port's protocol if needed.
	protocolChecker fetchers.ProtocolChecker
	// DiscoveryGroup is the name of the discovery group that the current
	// discovery service is a part of.
	// It is used to filter out discovered resources that belong to another
	// discovery services. When running in high availability mode and the agents
	// have access to the same cloud resources, this field value must be the same
	// for all discovery services. If different agents are used to discover different
	// sets of cloud resources, this field must be different for each set of agents.
	DiscoveryGroup string
	// ClusterName is the name of the Teleport cluster.
	ClusterName string
	// PollInterval is the cadence at which the discovery server will run each of its
	// discovery cycles.
	PollInterval time.Duration

	// clock is passed to watchers to handle poll intervals.
	// Mostly used in tests.
	clock clockwork.Clock
}

func (c *Config) CheckAndSetDefaults() error {
	if c.Matchers.IsEmpty() && c.DiscoveryGroup == "" {
		return trace.BadParameter("no matchers or discovery group configured for discovery")
	}
	if c.Emitter == nil {
		return trace.BadParameter("no Emitter configured for discovery")
	}
	if c.AccessPoint == nil {
		return trace.BadParameter("no AccessPoint configured for discovery")
	}

	if len(c.Matchers.Kubernetes) > 0 && c.DiscoveryGroup == "" {
		return trace.BadParameter(`the DiscoveryGroup name should be set for discovery server if
kubernetes matchers are present.`)
	}
	if c.CloudClients == nil {
		cloudClients, err := cloud.NewClients()
		if err != nil {
			return trace.Wrap(err, "unable to create cloud clients")
		}
		c.CloudClients = cloudClients
	}
	if c.KubernetesClient == nil && len(c.Matchers.Kubernetes) > 0 {
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return trace.Wrap(err,
				"the Kubernetes App Discovery requires a Teleport Kube Agent running on a Kubernetes cluster")
		}
		kubeClient, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return trace.Wrap(err, "unable to create Kubernetes client")
		}

		c.KubernetesClient = kubeClient
	}

	if c.Log == nil {
		c.Log = logrus.New()
	}
	if c.protocolChecker == nil {
		c.protocolChecker = fetchers.NewProtoChecker(false)
	}

	if c.PollInterval == 0 {
		c.PollInterval = 5 * time.Minute
	}

	if c.clock == nil {
		c.clock = clockwork.NewRealClock()
	}

	c.Log = c.Log.WithField(trace.Component, teleport.ComponentDiscovery)
	c.Matchers.Azure = services.SimplifyAzureMatchers(c.Matchers.Azure)
	return nil
}

// Server is a discovery server, used to discover cloud resources for
// inclusion in Teleport
type Server struct {
	*Config

	ctx context.Context
	// cancelfn is used with ctx when stopping the discovery server
	cancelfn context.CancelFunc
	// nodeWatcher is a node watcher.
	nodeWatcher *services.NodeWatcher

	// ec2Watcher periodically retrieves EC2 instances.
	ec2Watcher *server.Watcher
	// ec2Installer is used to start the installation process on discovered EC2 nodes
	ec2Installer ssmInstaller
	// azureWatcher periodically retrieves Azure virtual machines.
	azureWatcher *server.Watcher
	// azureInstaller is used to start the installation process on discovered Azure
	// virtual machines.
	azureInstaller azureInstaller
	// gcpWatcher periodically retrieves GCP virtual machines.
	gcpWatcher *server.Watcher
	// gcpInstaller is used to start the installation process on discovered GCP
	// virtual machines
	gcpInstaller gcpInstaller
	// kubeFetchers holds all kubernetes fetchers for Azure and other clouds.
	kubeFetchers []common.Fetcher
	// kubeAppsFetchers holds all kubernetes fetchers for apps.
	kubeAppsFetchers []common.Fetcher
	// databaseFetchers holds all database fetchers.
	databaseFetchers []common.Fetcher

	// dynamicMatcherWatcher is an initialized Watcher for DiscoveryConfig resources.
	// Each new event must update the existing resources.
	dynamicMatcherWatcher types.Watcher

	// dynamicDatabaseFetchers holds the current Database Fetchers for the Dynamic Matchers (those coming from DiscoveryConfig resource).
	// The key is the DiscoveryConfig name.
	dynamicDatabaseFetchers map[string][]common.Fetcher
	muDynamicFetchers       sync.RWMutex

	// caRotationCh receives nodes that need to have their CAs rotated.
	caRotationCh chan []types.Server
	// reconciler periodically reconciles the labels of discovered instances
	// with the auth server.
	reconciler *labelReconciler

	mu sync.Mutex
	// usageEventCache keeps track of which instances the server has emitted
	// usage events for.
	usageEventCache map[string]struct{}
}

// New initializes a discovery Server
func New(ctx context.Context, cfg *Config) (*Server, error) {
	if err := cfg.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}

	localCtx, cancelfn := context.WithCancel(ctx)
	s := &Server{
		Config:                  cfg,
		ctx:                     localCtx,
		cancelfn:                cancelfn,
		usageEventCache:         make(map[string]struct{}),
		dynamicDatabaseFetchers: make(map[string][]common.Fetcher),
	}

	if err := s.startDynamicMatchersWatcher(ctx); err != nil {
		return nil, trace.Wrap(err)
	}

	databaseFetchers, err := s.databaseFetchersFromMatchers(cfg.Matchers)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	s.databaseFetchers = databaseFetchers

	if err := s.initAWSWatchers(cfg.Matchers.AWS); err != nil {
		return nil, trace.Wrap(err)
	}

	if err := s.initAzureWatchers(ctx, cfg.Matchers.Azure); err != nil {
		return nil, trace.Wrap(err)
	}

	if err := s.initGCPWatchers(ctx, cfg.Matchers.GCP); err != nil {
		return nil, trace.Wrap(err)
	}

	if s.ec2Watcher != nil || s.azureWatcher != nil || s.gcpWatcher != nil {
		if err := s.initTeleportNodeWatcher(); err != nil {
			return nil, trace.Wrap(err)
		}
	}

	if err := s.initKubeAppWatchers(cfg.Matchers.Kubernetes); err != nil {
		return nil, trace.Wrap(err)
	}

	return s, nil
}

// startDynamicMatchersWatcher starts a watcher for DiscoveryConfig events.
// After initialization, it starts a goroutine that receives and handles events.
func (s *Server) startDynamicMatchersWatcher(ctx context.Context) error {
	if s.DiscoveryGroup == "" {
		return nil
	}

	watcher, err := s.AccessPoint.NewWatcher(ctx, types.Watch{
		Kinds: []types.WatchKind{{
			Kind: types.KindDiscoveryConfig,
		}},
	})
	if err != nil {
		return trace.Wrap(err)
	}

	// Wait for OpInit event so the watcher is ready.
	select {
	case event := <-watcher.Events():
		if event.Type != types.OpInit {
			return trace.BadParameter("failed to watch for DiscoveryConfig: received an unexpected event while waiting for the initial OpInit")
		}
	case <-watcher.Done():
		return trace.Wrap(watcher.Error())
	}

	s.dynamicMatcherWatcher = watcher

	go s.startDynamicWatcherUpdater()
	return nil
}

// initAWSWatchers starts AWS resource watchers based on types provided.
func (s *Server) initAWSWatchers(matchers []types.AWSMatcher) error {
	ec2Matchers, otherMatchers := splitMatchers(matchers, func(matcherType string) bool {
		return matcherType == types.AWSMatcherEC2
	})

	// start ec2 watchers
	var err error
	if len(ec2Matchers) > 0 {
		s.caRotationCh = make(chan []types.Server)
		s.ec2Watcher, err = server.NewEC2Watcher(s.ctx, ec2Matchers, s.CloudClients, s.caRotationCh, server.WithPollInterval(s.PollInterval))
		if err != nil {
			return trace.Wrap(err)
		}

		if s.ec2Installer == nil {
			s.ec2Installer = server.NewSSMInstaller(server.SSMInstallerConfig{
				Emitter: s.Emitter,
			})
		}

		lr, err := newLabelReconciler(&labelReconcilerConfig{
			log:         s.Log,
			accessPoint: s.AccessPoint,
		})
		if err != nil {
			return trace.Wrap(err)
		}
		s.reconciler = lr
	}

	// Database fetchers were added in databaseFetchersFromMatchers.
	_, otherMatchers = splitMatchers(otherMatchers, db.IsAWSMatcherType)

	// Add kube fetchers.
	for _, matcher := range otherMatchers {
		matcherAssumeRole := &types.AssumeRole{}
		if matcher.AssumeRole != nil {
			matcherAssumeRole = matcher.AssumeRole
		}

		for _, t := range matcher.Types {
			for _, region := range matcher.Regions {
				switch t {
				case types.AWSMatcherEKS:
					client, err := s.CloudClients.GetAWSEKSClient(
						s.ctx,
						region,
						cloud.WithAssumeRole(
							matcherAssumeRole.RoleARN,
							matcherAssumeRole.ExternalID,
						),
					)
					if err != nil {
						return trace.Wrap(err)
					}
					fetcher, err := fetchers.NewEKSFetcher(
						fetchers.EKSFetcherConfig{
							Client:       client,
							Region:       region,
							FilterLabels: matcher.Tags,
							Log:          s.Log,
						},
					)
					if err != nil {
						return trace.Wrap(err)
					}
					s.kubeFetchers = append(s.kubeFetchers, fetcher)
				}
			}
		}
	}

	return nil
}

func (s *Server) initKubeAppWatchers(matchers []types.KubernetesMatcher) error {
	if len(matchers) == 0 {
		return nil
	}

	kubeClient := s.KubernetesClient
	if kubeClient == nil {
		return trace.BadParameter("Kubernetes client is not present")
	}

	for _, matcher := range matchers {
		if !slices.Contains(matcher.Types, types.KubernetesMatchersApp) {
			continue
		}

		fetcher, err := fetchers.NewKubeAppsFetcher(fetchers.KubeAppsFetcherConfig{
			KubernetesClient: kubeClient,
			FilterLabels:     matcher.Labels,
			Namespaces:       matcher.Namespaces,
			Log:              s.Log,
			ClusterName:      s.DiscoveryGroup,
			ProtocolChecker:  s.Config.protocolChecker,
		})
		if err != nil {
			return trace.Wrap(err)
		}
		s.kubeAppsFetchers = append(s.kubeAppsFetchers, fetcher)
	}
	return nil
}

// databaseFetchersFromMatchers converts Matchers into a set of Database Fetchers.
func (s *Server) databaseFetchersFromMatchers(matchers Matchers) ([]common.Fetcher, error) {
	var fetchers []common.Fetcher

	// AWS
	awsDatabaseMatchers, _ := splitMatchers(matchers.AWS, db.IsAWSMatcherType)
	if len(awsDatabaseMatchers) > 0 {
		databaseFetchers, err := db.MakeAWSFetchers(s.ctx, s.CloudClients, awsDatabaseMatchers)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		fetchers = append(fetchers, databaseFetchers...)
	}

	// Azure
	azureDatabaseMatchers, _ := splitMatchers(matchers.Azure, db.IsAzureMatcherType)
	if len(azureDatabaseMatchers) > 0 {
		databaseFetchers, err := db.MakeAzureFetchers(s.CloudClients, azureDatabaseMatchers)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		fetchers = append(fetchers, databaseFetchers...)
	}

	// There are no Database Matchers for GCP Matchers.
	// There are no Database Matchers for Kube Matchers.

	return fetchers, nil
}

// initAzureWatchers starts Azure resource watchers based on types provided.
func (s *Server) initAzureWatchers(ctx context.Context, matchers []types.AzureMatcher) error {
	vmMatchers, otherMatchers := splitMatchers(matchers, func(matcherType string) bool {
		return matcherType == types.AzureMatcherVM
	})

	// VM watcher.
	if len(vmMatchers) > 0 {
		var err error
		s.azureWatcher, err = server.NewAzureWatcher(s.ctx, vmMatchers, s.CloudClients, server.WithPollInterval(s.PollInterval))
		if err != nil {
			return trace.Wrap(err)
		}
		if s.azureInstaller == nil {
			s.azureInstaller = &server.AzureInstaller{
				Emitter: s.Emitter,
			}
		}
	}

	// Database fetchers were added in databaseFetchersFromMatchers.
	_, otherMatchers = splitMatchers(otherMatchers, db.IsAzureMatcherType)

	// Add kube fetchers.
	for _, matcher := range otherMatchers {
		subscriptions, err := s.getAzureSubscriptions(ctx, matcher.Subscriptions)
		if err != nil {
			return trace.Wrap(err)
		}
		for _, subscription := range subscriptions {
			for _, t := range matcher.Types {
				switch t {
				case types.AzureMatcherKubernetes:
					kubeClient, err := s.CloudClients.GetAzureKubernetesClient(subscription)
					if err != nil {
						return trace.Wrap(err)
					}
					fetcher, err := fetchers.NewAKSFetcher(fetchers.AKSFetcherConfig{
						Client:         kubeClient,
						Regions:        matcher.Regions,
						FilterLabels:   matcher.ResourceTags,
						ResourceGroups: matcher.ResourceGroups,
						Log:            s.Log,
					})
					if err != nil {
						return trace.Wrap(err)
					}
					s.kubeFetchers = append(s.kubeFetchers, fetcher)
				}
			}
		}
	}
	return nil
}

// initGCPWatchers starts GCP resource watchers based on types provided.
func (s *Server) initGCPWatchers(ctx context.Context, matchers []types.GCPMatcher) error {
	// return early if there are no matchers as GetGCPGKEClient causes
	// an error if there are no credentials present
	if len(matchers) == 0 {
		return nil
	}

	vmMatchers, otherMatchers := splitMatchers(matchers, func(matcherType string) bool {
		return matcherType == types.GCPMatcherCompute
	})

	// VM watcher.
	if len(vmMatchers) > 0 {
		var err error
		s.gcpWatcher, err = server.NewGCPWatcher(s.ctx, vmMatchers, s.CloudClients)
		if err != nil {
			return trace.Wrap(err)
		}
		if s.gcpInstaller == nil {
			s.gcpInstaller = &server.GCPInstaller{
				Emitter: s.Emitter,
			}
		}
	}

	kubeClient, err := s.CloudClients.GetGCPGKEClient(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	for _, matcher := range otherMatchers {
		for _, projectID := range matcher.ProjectIDs {
			for _, location := range matcher.Locations {
				for _, t := range matcher.Types {
					switch t {
					case types.GCPMatcherKubernetes:
						fetcher, err := fetchers.NewGKEFetcher(fetchers.GKEFetcherConfig{
							Client:       kubeClient,
							Location:     location,
							FilterLabels: matcher.GetLabels(),
							ProjectID:    projectID,
							Log:          s.Log,
						})
						if err != nil {
							return trace.Wrap(err)
						}
						s.kubeFetchers = append(s.kubeFetchers, fetcher)
					}
				}
			}
		}
	}
	return nil
}

func (s *Server) filterExistingEC2Nodes(instances *server.EC2Instances) {
	nodes := s.nodeWatcher.GetNodes(s.ctx, func(n services.Node) bool {
		labels := n.GetAllLabels()
		_, accountOK := labels[types.AWSAccountIDLabel]
		_, instanceOK := labels[types.AWSInstanceIDLabel]
		return accountOK && instanceOK
	})

	var filtered []server.EC2Instance
outer:
	for _, inst := range instances.Instances {
		for _, node := range nodes {
			match := types.MatchLabels(node, map[string]string{
				types.AWSAccountIDLabel:  instances.AccountID,
				types.AWSInstanceIDLabel: inst.InstanceID,
			})
			if match {
				continue outer
			}
		}
		filtered = append(filtered, inst)
	}
	instances.Instances = filtered
}

func genEC2InstancesLogStr(instances []server.EC2Instance) string {
	return genInstancesLogStr(instances, func(i server.EC2Instance) string {
		return i.InstanceID
	})
}

func genAzureInstancesLogStr(instances []*armcompute.VirtualMachine) string {
	return genInstancesLogStr(instances, func(i *armcompute.VirtualMachine) string {
		return aws.StringValue(i.Name)
	})
}

func genGCPInstancesLogStr(instances []*gcp.Instance) string {
	return genInstancesLogStr(instances, func(i *gcp.Instance) string {
		return i.Name
	})
}

func genInstancesLogStr[T any](instances []T, getID func(T) string) string {
	var logInstances strings.Builder
	for idx, inst := range instances {
		if idx == 10 || idx == (len(instances)-1) {
			logInstances.WriteString(getID(inst))
			break
		}
		logInstances.WriteString(getID(inst) + ", ")
	}
	if len(instances) > 10 {
		logInstances.WriteString(fmt.Sprintf("... + %d instance IDs truncated", len(instances)-10))
	}

	return fmt.Sprintf("[%s]", logInstances.String())
}

func (s *Server) handleEC2Instances(instances *server.EC2Instances) error {
	// TODO(gavin): support assume_role_arn for ec2.
	ec2Client, err := s.CloudClients.GetAWSSSMClient(s.ctx, instances.Region)
	if err != nil {
		return trace.Wrap(err)
	}

	serverInfos, err := instances.ServerInfos()
	if err != nil {
		return trace.Wrap(err)
	}
	s.reconciler.queueServerInfos(serverInfos)

	// instances.Rotation is true whenever the instances received need
	// to be rotated, we don't want to filter out existing OpenSSH nodes as
	// they all need to have the command run on them
	if !instances.Rotation {
		s.filterExistingEC2Nodes(instances)
	}
	if len(instances.Instances) == 0 {
		return trace.NotFound("all fetched nodes already enrolled")
	}

	s.Log.Debugf("Running Teleport installation on these instances: AccountID: %s, Instances: %s",
		instances.AccountID, genEC2InstancesLogStr(instances.Instances))

	req := server.SSMRunRequest{
		DocumentName: instances.DocumentName,
		SSM:          ec2Client,
		Instances:    instances.Instances,
		Params:       instances.Parameters,
		Region:       instances.Region,
		AccountID:    instances.AccountID,
	}
	if err := s.ec2Installer.Run(s.ctx, req); err != nil {
		return trace.Wrap(err)
	}
	if err := s.emitUsageEvents(instances.MakeEvents()); err != nil {
		s.Log.WithError(err).Debug("Error emitting usage event.")
	}
	return nil
}

func (s *Server) logHandleInstancesErr(err error) {
	var aErr awserr.Error
	if errors.As(err, &aErr) && aErr.Code() == ssm.ErrCodeInvalidInstanceId {
		s.Log.WithError(err).Error("SSM SendCommand failed with ErrCodeInvalidInstanceId. Make sure that the instances have AmazonSSMManagedInstanceCore policy assigned. Also check that SSM agent is running and registered with the SSM endpoint on that instance and try restarting or reinstalling it in case of issues. See https://docs.aws.amazon.com/systems-manager/latest/APIReference/API_SendCommand.html#API_SendCommand_Errors for more details.")
	} else if trace.IsNotFound(err) {
		s.Log.Debug("All discovered EC2 instances are already part of the cluster.")
	} else {
		s.Log.WithError(err).Error("Failed to enroll discovered EC2 instances.")
	}
}

func (s *Server) watchCARotation(ctx context.Context) {
	ticker := time.NewTicker(time.Minute * 10)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			nodes, err := s.findUnrotatedEC2Nodes(ctx)
			if err != nil {
				if trace.IsNotFound(err) {
					s.Log.Debug("No OpenSSH nodes require CA rotation")
					continue
				}
				s.Log.Errorf("Error finding OpenSSH nodes requiring CA rotation: %s", err)
				continue
			}
			s.Log.Debugf("Found %d nodes requiring rotation", len(nodes))
			s.caRotationCh <- nodes
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *Server) getMostRecentRotationForCAs(ctx context.Context, caTypes ...types.CertAuthType) (time.Time, error) {
	var mostRecentUpdate time.Time
	for _, caType := range caTypes {
		ca, err := s.AccessPoint.GetCertAuthority(ctx, types.CertAuthID{
			Type:       caType,
			DomainName: s.ClusterName,
		}, false)
		if err != nil {
			return time.Time{}, trace.Wrap(err)
		}
		caRot := ca.GetRotation()
		if caRot.State == types.RotationStateInProgress && caRot.Started.After(mostRecentUpdate) {
			mostRecentUpdate = caRot.Started
		}

		if caRot.LastRotated.After(mostRecentUpdate) {
			mostRecentUpdate = caRot.LastRotated
		}
	}
	return mostRecentUpdate, nil
}

func (s *Server) findUnrotatedEC2Nodes(ctx context.Context) ([]types.Server, error) {
	mostRecentCertRotation, err := s.getMostRecentRotationForCAs(ctx, types.OpenSSHCA, types.HostCA)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	found := s.nodeWatcher.GetNodes(ctx, func(n services.Node) bool {
		if n.GetSubKind() != types.SubKindOpenSSHNode {
			return false
		}
		if _, ok := n.GetLabel(types.AWSAccountIDLabel); !ok {
			return false
		}
		if _, ok := n.GetLabel(types.AWSInstanceIDLabel); !ok {
			return false
		}

		return mostRecentCertRotation.After(n.GetRotation().LastRotated)
	})

	if len(found) == 0 {
		return nil, trace.NotFound("no unrotated nodes found")
	}
	return found, nil
}

func (s *Server) handleEC2Discovery() {
	if err := s.nodeWatcher.WaitInitialization(); err != nil {
		s.Log.WithError(err).Error("Failed to initialize nodeWatcher.")
		return
	}

	go s.ec2Watcher.Run()
	go s.watchCARotation(s.ctx)

	for {
		select {
		case instances := <-s.ec2Watcher.InstancesC:
			ec2Instances := instances.EC2
			s.Log.Debugf("EC2 instances discovered (AccountID: %s, Instances: %v), starting installation",
				ec2Instances.AccountID, genEC2InstancesLogStr(ec2Instances.Instances))

			if err := s.handleEC2Instances(ec2Instances); err != nil {
				s.logHandleInstancesErr(err)
			}
		case <-s.ctx.Done():
			s.ec2Watcher.Stop()
			return
		}
	}
}

func (s *Server) filterExistingAzureNodes(instances *server.AzureInstances) {
	nodes := s.nodeWatcher.GetNodes(s.ctx, func(n services.Node) bool {
		labels := n.GetAllLabels()
		_, subscriptionOK := labels[types.SubscriptionIDLabel]
		_, vmOK := labels[types.VMIDLabel]
		return subscriptionOK && vmOK
	})
	var filtered []*armcompute.VirtualMachine
outer:
	for _, inst := range instances.Instances {
		for _, node := range nodes {
			var vmID string
			if inst.Properties != nil {
				vmID = aws.StringValue(inst.Properties.VMID)
			}
			match := types.MatchLabels(node, map[string]string{
				types.SubscriptionIDLabel: instances.SubscriptionID,
				types.VMIDLabel:           vmID,
			})
			if match {
				continue outer
			}
		}
		filtered = append(filtered, inst)
	}
	instances.Instances = filtered
}

func (s *Server) handleAzureInstances(instances *server.AzureInstances) error {
	client, err := s.CloudClients.GetAzureRunCommandClient(instances.SubscriptionID)
	if err != nil {
		return trace.Wrap(err)
	}
	s.filterExistingAzureNodes(instances)
	if len(instances.Instances) == 0 {
		return trace.Wrap(errNoInstances)
	}

	s.Log.Debugf("Running Teleport installation on these virtual machines: SubscriptionID: %s, VMs: %s",
		instances.SubscriptionID, genAzureInstancesLogStr(instances.Instances),
	)
	req := server.AzureRunRequest{
		Client:          client,
		Instances:       instances.Instances,
		Region:          instances.Region,
		ResourceGroup:   instances.ResourceGroup,
		Params:          instances.Parameters,
		ScriptName:      instances.ScriptName,
		PublicProxyAddr: instances.PublicProxyAddr,
		ClientID:        instances.ClientID,
	}
	if err := s.azureInstaller.Run(s.ctx, req); err != nil {
		return trace.Wrap(err)
	}
	if err := s.emitUsageEvents(instances.MakeEvents()); err != nil {
		s.Log.WithError(err).Debug("Error emitting usage event.")
	}
	return nil
}

func (s *Server) handleAzureDiscovery() {
	if err := s.nodeWatcher.WaitInitialization(); err != nil {
		s.Log.WithError(err).Error("Failed to initialize nodeWatcher.")
		return
	}

	go s.azureWatcher.Run()
	for {
		select {
		case instances := <-s.azureWatcher.InstancesC:
			azureInstances := instances.Azure
			s.Log.Debugf("Azure instances discovered (SubscriptionID: %s, Instances: %v), starting installation",
				azureInstances.SubscriptionID, genAzureInstancesLogStr(azureInstances.Instances),
			)
			if err := s.handleAzureInstances(azureInstances); err != nil {
				if errors.Is(err, errNoInstances) {
					s.Log.Debug("All discovered Azure VMs are already part of the cluster.")
				} else {
					s.Log.WithError(err).Error("Failed to enroll discovered Azure VMs.")
				}
			}
		case <-s.ctx.Done():
			s.azureWatcher.Stop()
			return
		}
	}
}

func (s *Server) filterExistingGCPNodes(instances *server.GCPInstances) {
	nodes := s.nodeWatcher.GetNodes(s.ctx, func(n services.Node) bool {
		labels := n.GetAllLabels()
		_, projectIDOK := labels[types.ProjectIDLabel]
		_, zoneOK := labels[types.ZoneLabel]
		_, nameOK := labels[types.NameLabel]
		return projectIDOK && zoneOK && nameOK
	})
	var filtered []*gcp.Instance
outer:
	for _, inst := range instances.Instances {
		for _, node := range nodes {
			match := types.MatchLabels(node, map[string]string{
				types.ProjectIDLabel: inst.ProjectID,
				types.ZoneLabel:      inst.Zone,
				types.NameLabel:      inst.Name,
			})
			if match {
				continue outer
			}
		}
		filtered = append(filtered, inst)
	}
	instances.Instances = filtered
}

func (s *Server) handleGCPInstances(instances *server.GCPInstances) error {
	client, err := s.CloudClients.GetGCPInstancesClient(s.ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	s.filterExistingGCPNodes(instances)
	if len(instances.Instances) == 0 {
		return trace.Wrap(errNoInstances)
	}

	s.Log.Debugf("Running Teleport installation on these virtual machines: ProjectID: %s, VMs: %s",
		instances.ProjectID, genGCPInstancesLogStr(instances.Instances),
	)
	req := server.GCPRunRequest{
		Client:          client,
		Instances:       instances.Instances,
		ProjectID:       instances.ProjectID,
		Zone:            instances.Zone,
		Params:          instances.Parameters,
		ScriptName:      instances.ScriptName,
		PublicProxyAddr: instances.PublicProxyAddr,
	}
	if err := s.gcpInstaller.Run(s.ctx, req); err != nil {
		return trace.Wrap(err)
	}
	if err := s.emitUsageEvents(instances.MakeEvents()); err != nil {
		s.Log.WithError(err).Debug("Error emitting usage event.")
	}
	return nil
}

func (s *Server) handleGCPDiscovery() {
	if err := s.nodeWatcher.WaitInitialization(); err != nil {
		s.Log.WithError(err).Error("Failed to initialize nodeWatcher.")
		return
	}
	go s.gcpWatcher.Run()
	for {
		select {
		case instances := <-s.gcpWatcher.InstancesC:
			gcpInstances := instances.GCP
			s.Log.Debugf("GCP instances discovered (ProjectID: %s, Instances %v), starting installation",
				gcpInstances.ProjectID, genGCPInstancesLogStr(gcpInstances.Instances),
			)
			if err := s.handleGCPInstances(gcpInstances); err != nil {
				if errors.Is(err, errNoInstances) {
					s.Log.Debug("All discovered GCP VMs are already part of the cluster.")
				} else {
					s.Log.WithError(err).Error("Failed to enroll discovered GCP VMs.")
				}
			}
		case <-s.ctx.Done():
			s.gcpWatcher.Stop()
			return
		}
	}
}

func (s *Server) emitUsageEvents(events map[string]*usageeventsv1.ResourceCreateEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, event := range events {
		if _, exists := s.usageEventCache[name]; exists {
			continue
		}
		s.usageEventCache[name] = struct{}{}
		if err := s.AccessPoint.SubmitUsageEvent(s.ctx, &proto.SubmitUsageEventRequest{
			Event: &usageeventsv1.UsageEventOneOf{
				Event: &usageeventsv1.UsageEventOneOf_ResourceCreateEvent{
					ResourceCreateEvent: event,
				},
			},
		}); err != nil {
			return trace.Wrap(err)
		}
	}
	return nil
}

// Start starts the discovery service.
func (s *Server) Start() error {
	if s.ec2Watcher != nil {
		go s.handleEC2Discovery()
		go s.reconciler.run(s.ctx)
	}
	if s.azureWatcher != nil {
		go s.handleAzureDiscovery()
	}
	if s.gcpWatcher != nil {
		go s.handleGCPDiscovery()
	}
	if err := s.startKubeWatchers(); err != nil {
		return trace.Wrap(err)
	}
	if err := s.startKubeAppsWatchers(); err != nil {
		return trace.Wrap(err)
	}
	if err := s.startDatabaseWatchers(); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// startDynamicWatcherUpdater watches for DiscoveryConfig resource change events.
// Before consuming changes, it iterates over all DiscoveryConfigs and
// For deleted resources, it deletes the matchers.
// For new/updated resources, it replaces the set of fetchers.
func (s *Server) startDynamicWatcherUpdater() {
	// Add all existing DiscoveryConfigs as matchers.
	nextKey := ""
	for {
		dcs, respNextKey, err := s.AccessPoint.ListDiscoveryConfigs(s.ctx, 0, nextKey)
		if err != nil {
			s.Log.WithError(err).Warnf("failed to list discovery configs")
			return
		}
		for _, dc := range dcs {
			if dc.GetDiscoveryGroup() != s.DiscoveryGroup {
				continue
			}

			if err := s.upsertDynamicMatchers(dc); err != nil {
				s.Log.WithError(err).Warnf("failed to update dynamic matchers for discovery config %q", dc.GetName())
				continue
			}
		}
		if respNextKey == "" {
			break
		}
		nextKey = respNextKey
	}

	// Consume DiscoveryConfig events to update Matchers as they change.
	for {
		select {
		case event := <-s.dynamicMatcherWatcher.Events():
			switch event.Type {
			case types.OpPut:
				dc, ok := event.Resource.(*discoveryconfig.DiscoveryConfig)
				if !ok {
					s.Log.Warnf("dynamic matcher watcher: unexpected resource type %T", event.Resource)
					return
				}

				if dc.GetDiscoveryGroup() != s.DiscoveryGroup {
					// Let's assume there's a DiscoveryConfig DC1 has DiscoveryGroup DG1, which this process is monitoring.
					// If the user updates the DiscoveryGroup to DG2, then DC1 must be removed from the scope of this process.
					// We blindly delete it, in the worst case, this is a no-op.
					s.muDynamicFetchers.Lock()
					delete(s.dynamicDatabaseFetchers, event.Resource.GetName())
					s.muDynamicFetchers.Unlock()
					continue
				}

				if err := s.upsertDynamicMatchers(dc); err != nil {
					s.Log.WithError(err).Warnf("failed to update dynamic matchers for discovery config %q", dc.GetName())
					continue
				}

			case types.OpDelete:
				s.muDynamicFetchers.Lock()
				delete(s.dynamicDatabaseFetchers, event.Resource.GetName())
				s.muDynamicFetchers.Unlock()

			default:
				s.Log.Warnf("Skipping unknown event type %s", event.Type)
			}
		case <-s.dynamicMatcherWatcher.Done():
			s.Log.Warnf("dynamic matcher watcher error: %v", s.dynamicMatcherWatcher.Error())
			return
		}
	}
}

// upsertDynamicMatchers upserts the internal set of dynamic matchers given a particular discovery config.
func (s *Server) upsertDynamicMatchers(dc *discoveryconfig.DiscoveryConfig) error {
	matchers := Matchers{
		AWS:        dc.Spec.AWS,
		Azure:      dc.Spec.Azure,
		GCP:        dc.Spec.GCP,
		Kubernetes: dc.Spec.Kube,
	}

	databaseFetchers, err := s.databaseFetchersFromMatchers(matchers)
	if err != nil {
		return trace.Wrap(err)
	}

	// TODO(marco): add other matcher types (VMs, Kubes, KubeApps)

	s.muDynamicFetchers.Lock()
	s.dynamicDatabaseFetchers[dc.GetName()] = databaseFetchers
	s.muDynamicFetchers.Unlock()
	return nil
}

// Stop stops the discovery service.
func (s *Server) Stop() {
	s.cancelfn()
	if s.ec2Watcher != nil {
		s.ec2Watcher.Stop()
	}
	if s.azureWatcher != nil {
		s.azureWatcher.Stop()
	}
	if s.gcpWatcher != nil {
		s.gcpWatcher.Stop()
	}
	if s.dynamicMatcherWatcher != nil {
		if err := s.dynamicMatcherWatcher.Close(); err != nil {
			s.Log.Warnf("dynamic matcher watcher closing error: ", trace.Wrap(err))
		}
	}
}

// Wait will block while the server is running.
func (s *Server) Wait() error {
	<-s.ctx.Done()
	if err := s.ctx.Err(); err != nil && err != context.Canceled {
		return trace.Wrap(err)
	}
	return nil
}

func (s *Server) getAzureSubscriptions(ctx context.Context, subs []string) ([]string, error) {
	subscriptionIds := subs
	if slices.Contains(subs, types.Wildcard) {
		subsClient, err := s.CloudClients.GetAzureSubscriptionClient()
		if err != nil {
			return nil, trace.Wrap(err)
		}
		subscriptionIds, err = subsClient.ListSubscriptionIDs(ctx)
		return subscriptionIds, trace.Wrap(err)
	}

	return subscriptionIds, nil
}

func (s *Server) initTeleportNodeWatcher() (err error) {
	s.nodeWatcher, err = services.NewNodeWatcher(s.ctx, services.NodeWatcherConfig{
		ResourceWatcherConfig: services.ResourceWatcherConfig{
			Component:    teleport.ComponentDiscovery,
			Log:          s.Log,
			Client:       s.AccessPoint,
			MaxStaleness: time.Minute,
		},
	})

	return trace.Wrap(err)
}

// splitSlice splits a slice into two, by putting all elements that satisfy the
// provided check function in the first slice, while putting all other elements
// in the second slice.
func splitSlice(ss []string, check func(string) bool) (split, other []string) {
	for _, e := range ss {
		if check(e) {
			split = append(split, e)
		} else {
			other = append(other, e)
		}
	}
	return
}

// splitMatchers splits a set of matchers by checking the matcher type.
func splitMatchers[T types.Matcher](matchers []T, matcherTypeCheck func(string) bool) (split, other []T) {
	for _, matcher := range matchers {
		splitTypes, otherTypes := splitSlice(matcher.GetTypes(), matcherTypeCheck)

		if len(splitTypes) > 0 {
			newMatcher := matcher.CopyWithTypes(splitTypes).(T)
			split = append(split, newMatcher)
		}
		if len(otherTypes) > 0 {
			newMatcher := matcher.CopyWithTypes(otherTypes).(T)
			other = append(other, newMatcher)
		}
	}
	return
}
