package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"

	raw "github.com/linode/linodego"
	"github.com/linode/linodego/k8s"
	k8scondition "github.com/linode/linodego/k8s/pkg/condition"
	"github.com/rancher/kontainer-engine/drivers/options"
	"github.com/rancher/kontainer-engine/drivers/util"
	"github.com/rancher/kontainer-engine/types"
	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
)

// DefaultLinodeURL is the Linode APIv4 URL to use
const DefaultLinodeURL = "https://api.linode.com/v4"
const retryInterval = 5 * time.Second

// Driver defines the struct of gke driver
type Driver struct {
	driverCapabilities types.Capabilities
}

type state struct {
	AccessToken string

	// The name of this cluster
	Name  string
	Label string
	// An optional description of this cluster
	Description string

	// The region to launch the cluster
	Region string
	// The kubernetes version
	K8sVersion string
	// Label      string // name ?
	Tags      []string
	NodePools map[string]int // type -> count

	// cluster info
	ClusterInfo types.ClusterInfo
}

func NewDriver() types.Driver {
	driver := &Driver{
		driverCapabilities: types.Capabilities{
			Capabilities: make(map[int64]bool),
		},
	}

	driver.driverCapabilities.AddCapability(types.GetVersionCapability)
	driver.driverCapabilities.AddCapability(types.SetVersionCapability)
	driver.driverCapabilities.AddCapability(types.GetClusterSizeCapability)
	driver.driverCapabilities.AddCapability(types.SetClusterSizeCapability)

	return driver
}

// GetDriverCreateOptions implements driver interface
func (d *Driver) GetDriverCreateOptions(ctx context.Context) (*types.DriverFlags, error) {
	driverFlag := types.DriverFlags{
		Options: make(map[string]*types.Flag),
	}

	driverFlag.Options["access-token"] = &types.Flag{
		Type:  types.StringType,
		Usage: "Linode api access token",
	}

	driverFlag.Options["name"] = &types.Flag{
		Type:  types.StringType,
		Usage: "the internal name of the cluster in Rancher",
	}
	driverFlag.Options["label"] = &types.Flag{
		Type:  types.StringType,
		Usage: "the label of the cluster in Linode",
	}
	driverFlag.Options["description"] = &types.Flag{
		Type:  types.StringType,
		Usage: "An optional description of this cluster",
	}

	driverFlag.Options["region"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The region to launch the cluster",
		Default: &types.Default{
			DefaultString: "us-central1-a",
		},
	}
	driverFlag.Options["tags"] = &types.Flag{
		Type:  types.StringSliceType,
		Usage: "The map of Kubernetes labels (key/value pairs) to be applied to each node",
	}
	driverFlag.Options["kubernetes-version"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The kubernetes version",
	}
	driverFlag.Options["node-pools"] = &types.Flag{
		Type:  types.StringSliceType,
		Usage: "The list of node pools created for the cluster",
	}

	return &driverFlag, nil
}

// GetDriverUpdateOptions implements driver interface
func (d *Driver) GetDriverUpdateOptions(ctx context.Context) (*types.DriverFlags, error) {
	driverFlag := types.DriverFlags{
		Options: make(map[string]*types.Flag),
	}
	driverFlag.Options["tags"] = &types.Flag{
		Type:  types.StringSliceType,
		Usage: "The map of Kubernetes labels (key/value pairs) to be applied to each node",
		Default: &types.Default{
			DefaultStringSlice: &types.StringSlice{
				Value: []string{},
			},
		},
	}
	driverFlag.Options["node-pools"] = &types.Flag{
		Type:  types.StringSliceType,
		Usage: "The list of node pools created for the cluster",
	}
	return &driverFlag, nil
}

// SetDriverOptions implements driver interface
func getStateFromOpts(driverOptions *types.DriverOptions) (state, error) {
	d := state{
		Tags:      []string{},
		NodePools: map[string]int{},
		ClusterInfo: types.ClusterInfo{
			Metadata: map[string]string{},
		},
	}

	d.Name = options.GetValueFromDriverOptions(driverOptions, types.StringType, "name").(string)
	d.Label = options.GetValueFromDriverOptions(driverOptions, types.StringType, "label").(string)
	d.Description = options.GetValueFromDriverOptions(driverOptions, types.StringType, "description").(string)

	d.AccessToken = options.GetValueFromDriverOptions(driverOptions, types.StringType, "access-token", "accessToken").(string)

	d.Region = options.GetValueFromDriverOptions(driverOptions, types.StringType, "region").(string)
	d.K8sVersion = options.GetValueFromDriverOptions(driverOptions, types.StringType, "kubernetes-version", "kubernetesVersion").(string)

	d.Tags = []string{}
	tags := options.GetValueFromDriverOptions(driverOptions, types.StringSliceType, "tags").(*types.StringSlice)
	for _, tag := range tags.Value {
		d.Tags = append(d.Tags, tag)
	}

	pools := options.GetValueFromDriverOptions(driverOptions, types.StringSliceType, "node-pools", "nodePools").(*types.StringSlice)
	for _, part := range pools.Value {
		kv := strings.Split(part, "=")
		if len(kv) == 2 {
			count, err := strconv.Atoi(kv[1])
			if err != nil {
				return state{}, fmt.Errorf("failed to parse node count %v for pool of node type %s", kv[1], kv[0])
			}
			d.NodePools[kv[0]] = count
		}
	}

	return d, d.validate()
}

func (s *state) validate() error {
	if len(s.NodePools) == 0 {
		return fmt.Errorf("at least one NodePool is required")
	}
	for t, count := range s.NodePools {
		if count <= 0 {
			return fmt.Errorf("at least 1 node required for NodePool=%s", t)
		}
	}
	return nil
}

var lkeLog = filepath.Join("/tmp", "kontainer-engine-driver-linode", "test_logs.txt")

func init() {
	err := os.MkdirAll(filepath.Dir(lkeLog), 0755)
	if err != nil {
		panic(err)
	}
}

// Create implements driver interface
func (d *Driver) Create(ctx context.Context, opts *types.DriverOptions, _ *types.ClusterInfo) (*types.ClusterInfo, error) {
	//var buf bytes.Buffer
	//if opts.StringOptions != nil {
	//	for k, v := range opts.StringOptions {
	//		buf.WriteString(k)
	//		buf.WriteString("=")
	//		buf.WriteString(v)
	//		buf.WriteString("[*]")
	//	}
	//}
	//return nil, errors.New(buf.String())

	state, err := getStateFromOpts(opts)
	if err != nil {
		return nil, err
	}

	logrus.Debugf("state.name %s, state: %#v", state.Name, state)
	ioutil.WriteFile(lkeLog, []byte(fmt.Sprintf("state.name %s, state: %#v\n", state.Name, state)), 0644)

	info := &types.ClusterInfo{}
	err = storeState(info, state)
	if err != nil {
		return info, err
	}

	client, err := d.getLinodeClient(ctx, state)
	if err != nil {
		return info, err
	}

	req := d.generateClusterCreateRequest(state)
	logrus.Debugf("LKE api request: %#v", req)
	ioutil.WriteFile(lkeLog, []byte(fmt.Sprintf("LKE api request: %#v", req)), 0644)

	cluster, err := CreateLKECluster(context.Background(), client, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create LKE cluster: %s", err)
	}
	info.Metadata["cluster-id"] = strconv.Itoa(cluster.ID)

	err = client.WaitForLKEClusterConditions(context.Background(), cluster.ID, raw.LKEClusterPollOptions{
		TimeoutSeconds: 10 * 60,
	}, k8scondition.ClusterHasReadyNode)
	if err != nil {
		return nil, err
	}
	return info, nil
}

// CreateLKECluster creates a LKECluster
func CreateLKECluster(ctx context.Context, c *raw.Client, createOpts raw.LKEClusterCreateOptions) (*raw.LKECluster, error) {
	var body string
	e, err := c.LKEClusters.Endpoint()
	if err != nil {
		return nil, err
	}

	req := c.R(ctx).SetResult(&raw.LKECluster{})

	if bodyData, err := json.Marshal(createOpts); err == nil {
		body = string(bodyData)
	} else {
		return nil, raw.NewError(err)
	}

	r, err := coupleAPIErrors(req.
		SetBody(body).
		Post(e))

	if err != nil {
		return nil, err
	}
	return r.Result().(*raw.LKECluster), nil
}
func coupleAPIErrors(r *resty.Response, err error) (*resty.Response, error) {
	if err != nil {
		return nil, raw.NewError(err)
	}
	logResponse(r.RawResponse)
	if r.Error() != nil {
		// Check that response is of the correct content-type before unmarshalling
		expectedContentType := r.Request.Header.Get("Accept")
		responseContentType := r.Header().Get("Content-Type")

		// If the upstream Linode API server being fronted fails to respond to the request,
		// the http server will respond with a default "Bad Gateway" page with Content-Type
		// "text/html".
		if r.StatusCode() == http.StatusBadGateway && responseContentType == "text/html" {
			return nil, raw.Error{Code: http.StatusBadGateway, Message: http.StatusText(http.StatusBadGateway)}
		}

		if responseContentType != expectedContentType {
			msg := fmt.Sprintf(
				"Unexpected Content-Type: Expected: %v, Received: %v",
				expectedContentType,
				responseContentType,
			)

			return nil, raw.NewError(msg)
		}

		apiError, ok := r.Error().(*raw.APIError)
		if !ok || (ok && len(apiError.Errors) == 0) {
			return r, nil
		}

		return nil, raw.NewError(r)
	}

	return r, nil
}

func storeState(info *types.ClusterInfo, state state) error {
	bytes, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if info.Metadata == nil {
		info.Metadata = map[string]string{}
	}
	info.Metadata["state"] = string(bytes)
	info.Metadata["region"] = state.Region
	return nil
}

func getState(info *types.ClusterInfo) (state, error) {
	state := state{}
	// ignore error
	err := json.Unmarshal([]byte(info.Metadata["state"]), &state)
	return state, err
}

// Update implements driver interface
func (d *Driver) Update(ctx context.Context, info *types.ClusterInfo, opts *types.DriverOptions) (*types.ClusterInfo, error) {
	state, err := getState(info)
	if err != nil {
		return nil, err
	}

	logrus.Debugf("state.name %s, state: %#v", state.Name, state)

	newState, err := getStateFromOpts(opts)
	if err != nil {
		return nil, err
	}

	client, err := d.getLinodeClient(ctx, state)
	if err != nil {
		return nil, err
	}

	clusterID, err := strconv.Atoi(info.Metadata["cluster-id"])
	if err != nil {
		return nil, fmt.Errorf("failed to parse cluster id: %s", err)
	}

	if state.Label != newState.Label || !sets.NewString(state.Tags...).Equal(sets.NewString(newState.Tags...)) {
		_, err = client.UpdateLKECluster(context.Background(), clusterID, raw.LKEClusterUpdateOptions{
			Label: newState.Label,
			Tags:  &newState.Tags,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to update cluster %d: %s", clusterID, err)
		}
		state.Tags = newState.Tags
	}

	pools, err := client.ListLKEClusterPools(context.Background(), clusterID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get pools for LKE cluster %d: %s", clusterID, err)
	}

	pm := make(map[string]raw.LKEClusterPool) // type -> pool
	for _, pool := range pools {
		if _, found := newState.NodePools[pool.Type]; !found {
			// delete
			err = client.DeleteLKEClusterPool(context.Background(), clusterID, pool.ID)
			if err != nil {
				return nil, fmt.Errorf("failed to delete cluster %s node pool type %s", state.Name, pool.Type)
			}
		} else {
			pm[pool.Type] = pool // id, count
		}
		delete(state.NodePools, pool.Type)
	}

	for t, count := range newState.NodePools {
		if cur, ok := pm[t]; ok {
			if cur.Count != count {
				// update
				_, err = client.UpdateLKEClusterPool(context.Background(), clusterID, cur.ID, raw.LKEClusterPoolUpdateOptions{
					Count: count,
				})
				if err != nil {
					return nil, fmt.Errorf("failed to update cluster %s node pool type %s", state.Name, cur.Type)
				}
				state.NodePools[t] = count
			}
		} else {
			// create
			_, err := client.CreateLKEClusterPool(context.Background(), clusterID, raw.LKEClusterPoolCreateOptions{
				Count: count,
				Type:  t,
				// Disks: nil, // not supported?
			})
			if err != nil {
				return nil, fmt.Errorf("failed to create cluster %s node pool type %s", state.Name, cur.Type)
			}
			state.NodePools[t] = count
		}
	}

	pools, err = client.ListLKEClusterPools(context.Background(), clusterID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get pools for LKE cluster %d: %s", clusterID, err)
	}
	for _, pool := range pools {
		err = waitUntilPoolReady(client, clusterID, pool.ID)
		if err != nil {
			return nil, err
		}
	}

	return info, storeState(info, state)
}

func (d *Driver) generateClusterCreateRequest(state state) raw.LKEClusterCreateOptions {
	req := raw.LKEClusterCreateOptions{
		Label:      state.Label,
		Region:     state.Region,
		K8sVersion: state.K8sVersion,
		Tags:       state.Tags,
	}
	for t, count := range state.NodePools {
		req.NodePools = append(req.NodePools, raw.LKEClusterPoolCreateOptions{
			Type:  t,
			Count: count,
			// Disks: nil, // unsupported?
		})
	}
	return req
}

func (d *Driver) PostCheck(ctx context.Context, info *types.ClusterInfo) (*types.ClusterInfo, error) {
	state, err := getState(info)
	if err != nil {
		ioutil.WriteFile(lkeLog, []byte(fmt.Sprintf("L364:: err: %s", err)), 0644)
		return nil, err
	}
	ioutil.WriteFile(lkeLog, []byte(fmt.Sprintf("L366:: state: %#v", state)), 0644)

	client, err := d.getLinodeClient(ctx, state)
	if err != nil {
		ioutil.WriteFile(lkeLog, []byte(fmt.Sprintf("L371:: err: %s", err)), 0644)
		return nil, err
	}

	clusterID, err := strconv.Atoi(info.Metadata["cluster-id"])
	if err != nil {
		ioutil.WriteFile(lkeLog, []byte(fmt.Sprintf("L377:: err: %s", err)), 0644)
		return nil, fmt.Errorf("failed to parse cluster id: %s", err)
	}
	ioutil.WriteFile(lkeLog, []byte(fmt.Sprintf("L380:: cluster-id: %#v", clusterID)), 0644)

	err = client.WaitForLKEClusterConditions(context.Background(), clusterID, raw.LKEClusterPollOptions{
		TimeoutSeconds: 10 * 60,
	}, k8scondition.ClusterHasReadyNode)
	if err != nil {
		ioutil.WriteFile(lkeLog, []byte(fmt.Sprintf("L386:: err: %s", err)), 0644)
		return nil, err
	}
	ioutil.WriteFile(lkeLog, []byte(fmt.Sprintf("L389:: cluster-id: %#v", clusterID)), 0644)

	lkeKubeconfig, err := client.GetLKEClusterKubeconfig(context.Background(), clusterID)
	if err != nil {
		ioutil.WriteFile(lkeLog, []byte(fmt.Sprintf("L393:: err: %s", err)), 0644)
		return nil, fmt.Errorf("failed to get kubeconfig for LKE cluster %d: %s", clusterID, err)
	}

	kubeConfigBytes, err := base64.StdEncoding.DecodeString(lkeKubeconfig.KubeConfig)
	if err != nil {
		ioutil.WriteFile(lkeLog, []byte(fmt.Sprintf("L399:: err: %s", err)), 0644)
		return nil, fmt.Errorf("failed to decode kubeconfig: %s", err)
	}

	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeConfigBytes)
	if err != nil {
		ioutil.WriteFile(lkeLog, []byte(fmt.Sprintf("L405:: err: %s", err)), 0644)
		return nil, fmt.Errorf("failed to parse LKE cluster kubeconfig: %s", err)
	}

	info.Version = state.K8sVersion
	count := 0
	for _, poolSize := range state.NodePools {
		count += poolSize
	}
	info.NodeCount = int64(count)

	info.Endpoint = cfg.Host
	info.Username = cfg.Username
	info.Password = cfg.Password
	if len(cfg.CAData) > 0 {
		info.RootCaCertificate = base64.StdEncoding.EncodeToString(cfg.CAData)
	}
	if len(cfg.CertData) > 0 {
		info.ClientCertificate = base64.StdEncoding.EncodeToString(cfg.CertData)
	}
	if len(cfg.KeyData) > 0 {
		info.ClientKey = base64.StdEncoding.EncodeToString(cfg.KeyData)
	}

	info.Metadata["KubeConfig"] = lkeKubeconfig.KubeConfig
	serviceAccountToken, err := generateServiceAccountTokenForLKE(lkeKubeconfig)
	if err != nil {
		ioutil.WriteFile(lkeLog, []byte(fmt.Sprintf("L432:: err: %s", err)), 0644)
		return nil, err
	}
	info.ServiceAccountToken = serviceAccountToken
	ioutil.WriteFile(lkeLog, []byte(fmt.Sprintf("L437:: ServiceAccountToken: %s", serviceAccountToken)), 0644)
	return info, nil
}

// Remove implements driver interface
func (d *Driver) Remove(ctx context.Context, info *types.ClusterInfo) error {
	state, err := getState(info)
	if err != nil {
		return err
	}

	client, err := d.getLinodeClient(ctx, state)
	if err != nil {
		return err
	}

	clusterID, err := strconv.Atoi(info.Metadata["cluster-id"])
	if err != nil {
		return fmt.Errorf("failed to parse cluster id: %s", err)
	}

	logrus.Debugf("Removing cluster %v from zone %v", state.Name, state.Region)

	err = client.DeleteLKECluster(context.Background(), clusterID)
	if err != nil {
		return fmt.Errorf("failed to delete Linode LKE cluster %d: %s", clusterID, err)
	}
	_, err = client.WaitForLKEClusterStatus(context.Background(), clusterID, "not_ready", 10*60)
	if err != nil {
		return err
	}

	return nil
}

func (d *Driver) getLinodeClient(ctx context.Context, state state) (*raw.Client, error) {
	// Bearer credID=cattle-global-data:cc-rdk9n passwordField=token
	tokenSource := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: fmt.Sprintf("credID=%s passwordField=token", state.AccessToken)})
	oauthTransport := &Transport{
		Source: tokenSource,
	}
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		oauthTransport.Base = transport
	}
	oauth2Client := &http.Client{
		Transport: oauthTransport,
	}
	client := raw.NewClient(oauth2Client)
	client.SetUserAgent("kontainer-engine-driver-linode")

	// https://localhost/meta/proxy/api.linode.com/v4/lke/versions
	client.SetBaseURL("https://localhost/meta/proxy/api.linode.com/v4")

	// const DefaultLinodeURL = "https://api.linode.com/v4"
	// client.SetBaseURL(DefaultLinodeURL)

	return &client, nil
}

func generateServiceAccountTokenForLKE(lkeKubeconfig *raw.LKEClusterKubeconfig) (string, error) {
	clientset, err := k8s.BuildClientsetFromConfig(lkeKubeconfig, nil)
	if err != nil {
		return "", err
	}

	return util.GenerateServiceAccountToken(clientset)
}

func (d *Driver) GetClusterSize(ctx context.Context, info *types.ClusterInfo) (*types.NodeCount, error) {
	state, err := getState(info)
	if err != nil {
		return nil, err
	}

	clusterID, err := strconv.Atoi(info.Metadata["cluster-id"])
	if err != nil {
		return nil, fmt.Errorf("failed to parse cluster id: %s", err)
	}

	client, err := d.getLinodeClient(ctx, state)
	if err != nil {
		return nil, err
	}

	pools, err := client.ListLKEClusterPools(context.Background(), clusterID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get pools for LKE cluster %d: %s", clusterID, err)
	}

	count := 0
	for _, pool := range pools {
		count += pool.Count
	}
	return &types.NodeCount{Count: int64(count)}, nil
}

func (d *Driver) GetVersion(ctx context.Context, info *types.ClusterInfo) (*types.KubernetesVersion, error) {
	state, err := getState(info)
	if err != nil {
		return nil, err
	}

	clusterID, err := strconv.Atoi(info.Metadata["cluster-id"])
	if err != nil {
		return nil, fmt.Errorf("failed to parse cluster id: %s", err)
	}

	client, err := d.getLinodeClient(ctx, state)
	if err != nil {
		return nil, err
	}

	cluster, err := client.GetLKECluster(context.Background(), clusterID)
	if err != nil {
		return nil, fmt.Errorf("failed to get LKE cluster %d: %s", clusterID, err)
	}
	return &types.KubernetesVersion{Version: cluster.K8sVersion}, nil
}

func (d *Driver) SetClusterSize(ctx context.Context, info *types.ClusterInfo, count *types.NodeCount) error {
	state, err := getState(info)
	if err != nil {
		return err
	}

	clusterID, err := strconv.Atoi(info.Metadata["cluster-id"])
	if err != nil {
		return fmt.Errorf("failed to parse cluster id: %s", err)
	}

	client, err := d.getLinodeClient(ctx, state)
	if err != nil {
		return err
	}

	logrus.Info("updating cluster size")

	pools, err := client.ListLKEClusterPools(context.Background(), clusterID, nil)
	if err != nil {
		return fmt.Errorf("failed to get pools for LKE cluster %d: %s", clusterID, err)
	}

	poolID := pools[0].ID
	poolNodeCount := pools[0].Count

	_, err = client.UpdateLKEClusterPool(context.Background(), clusterID, poolID, raw.LKEClusterPoolUpdateOptions{
		Count: int(count.Count),
	})

	if poolNodeCount < int(count.Count) {
		err = waitUntilPoolReady(client, clusterID, poolID)
		if err != nil {
			return err
		}
	}

	logrus.Info("cluster size updated successfully")

	return nil
}

func waitUntilPoolReady(client *raw.Client, clusterID int, poolID int) error {
	return wait.PollImmediateInfinite(retryInterval, func() (done bool, err error) {
		pool, err := client.GetLKEClusterPool(context.Background(), clusterID, poolID)
		if err != nil {
			return false, err
		}
		for _, linode := range pool.Linodes {
			if linode.Status != raw.LKELinodeReady {
				return false, nil
			}
		}
		return true, nil
	})
}

func (d *Driver) SetVersion(ctx context.Context, info *types.ClusterInfo, version *types.KubernetesVersion) error {
	return nil
}

func (d *Driver) GetCapabilities(ctx context.Context) (*types.Capabilities, error) {
	return &d.driverCapabilities, nil
}

func (d *Driver) ETCDSave(ctx context.Context, clusterInfo *types.ClusterInfo, opts *types.DriverOptions, snapshotName string) error {
	return fmt.Errorf("ETCD backup operations are not implemented")
}

func (d *Driver) ETCDRestore(ctx context.Context, clusterInfo *types.ClusterInfo, opts *types.DriverOptions, snapshotName string) (*types.ClusterInfo, error) {
	return nil, fmt.Errorf("ETCD backup operations are not implemented")
}

func (d *Driver) ETCDRemoveSnapshot(ctx context.Context, clusterInfo *types.ClusterInfo, opts *types.DriverOptions, snapshotName string) error {
	return fmt.Errorf("ETCD backup operations are not implemented")
}

func (d *Driver) GetK8SCapabilities(ctx context.Context, options *types.DriverOptions) (*types.K8SCapabilities, error) {
	capabilities := &types.K8SCapabilities{
		L4LoadBalancer: &types.LoadBalancerCapabilities{
			Enabled:              true,
			Provider:             "NodeBalancer", // what are the options?
			ProtocolsSupported:   []string{"TCP", "UDP"},
			HealthCheckSupported: true,
		},
	}
	return capabilities, nil
}

func (d *Driver) RemoveLegacyServiceAccount(ctx context.Context, info *types.ClusterInfo) error {
	return nil
}
