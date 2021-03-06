package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/openshift/origin/pkg/api/apihelpers"
	internal "github.com/openshift/origin/pkg/cmd/server/apis/config"
)

func addDefaultingFuncs(scheme *runtime.Scheme) error {
	RegisterDefaults(scheme)
	return nil
}
func SetDefaults_MasterConfig(obj *MasterConfig) {
	if len(obj.APILevels) == 0 {
		obj.APILevels = internal.DefaultOpenShiftAPILevels
	}
	if len(obj.ControllerConfig.Controllers) == 0 {
		switch {
		case len(obj.Controllers) == 0 || obj.Controllers == ControllersAll:
			obj.ControllerConfig.Controllers = []string{"*"}
		case obj.Controllers == ControllersDisabled:
			obj.ControllerConfig.Controllers = []string{}
		}
	}
	if election := obj.ControllerConfig.Election; election != nil {
		if len(election.LockNamespace) == 0 {
			election.LockNamespace = "kube-system"
		}
		if len(election.LockResource.Group) == 0 && len(election.LockResource.Resource) == 0 {
			election.LockResource.Resource = "configmaps"
		}
	}

	if obj.ServingInfo.RequestTimeoutSeconds == 0 {
		obj.ServingInfo.RequestTimeoutSeconds = 60 * 60
	}
	if obj.ServingInfo.MaxRequestsInFlight == 0 {
		obj.ServingInfo.MaxRequestsInFlight = 1200
	}
	if len(obj.RoutingConfig.Subdomain) == 0 {
		obj.RoutingConfig.Subdomain = "router.default.svc.cluster.local"
	}
	if len(obj.JenkinsPipelineConfig.TemplateNamespace) == 0 {
		obj.JenkinsPipelineConfig.TemplateNamespace = "openshift"
	}
	if len(obj.JenkinsPipelineConfig.TemplateName) == 0 {
		obj.JenkinsPipelineConfig.TemplateName = "jenkins-ephemeral"
	}
	if len(obj.JenkinsPipelineConfig.ServiceName) == 0 {
		obj.JenkinsPipelineConfig.ServiceName = "jenkins"
	}
	if obj.JenkinsPipelineConfig.AutoProvisionEnabled == nil {
		v := true
		obj.JenkinsPipelineConfig.AutoProvisionEnabled = &v
	}

	if obj.MasterClients.OpenShiftLoopbackClientConnectionOverrides == nil {
		obj.MasterClients.OpenShiftLoopbackClientConnectionOverrides = &ClientConnectionOverrides{}
	}
	// historical values
	if obj.MasterClients.OpenShiftLoopbackClientConnectionOverrides.QPS <= 0 {
		obj.MasterClients.OpenShiftLoopbackClientConnectionOverrides.QPS = 150.0
	}
	if obj.MasterClients.OpenShiftLoopbackClientConnectionOverrides.Burst <= 0 {
		obj.MasterClients.OpenShiftLoopbackClientConnectionOverrides.Burst = 300
	}
	SetDefaults_ClientConnectionOverrides(obj.MasterClients.OpenShiftLoopbackClientConnectionOverrides)

	// Populate the new NetworkConfig.ServiceNetworkCIDR field from the KubernetesMasterConfig.ServicesSubnet field if needed
	if len(obj.NetworkConfig.ServiceNetworkCIDR) == 0 {
		if len(obj.KubernetesMasterConfig.ServicesSubnet) > 0 {
			// if a subnet is set in the kubernetes master config, use that
			obj.NetworkConfig.ServiceNetworkCIDR = obj.KubernetesMasterConfig.ServicesSubnet
		} else {
			// default ServiceClusterIPRange used by kubernetes if nothing is specified
			obj.NetworkConfig.ServiceNetworkCIDR = "10.0.0.0/24"
		}
	}

	// TODO Detect cloud provider when not using built-in kubernetes
	noCloudProvider := (len(obj.KubernetesMasterConfig.ControllerArguments["cloud-provider"]) == 0 || obj.KubernetesMasterConfig.ControllerArguments["cloud-provider"][0] == "")

	if noCloudProvider && len(obj.NetworkConfig.IngressIPNetworkCIDR) == 0 {
		cidr := internal.DefaultIngressIPNetworkCIDR
		cidrOverlap := false
		if internal.CIDRsOverlap(cidr, obj.NetworkConfig.ServiceNetworkCIDR) {
			cidrOverlap = true
		} else {
			for _, entry := range obj.NetworkConfig.ClusterNetworks {
				if internal.CIDRsOverlap(cidr, entry.CIDR) {
					cidrOverlap = true
					break
				}
			}
		}
		if !cidrOverlap {
			obj.NetworkConfig.IngressIPNetworkCIDR = cidr
		}
	}

	// Historically, the clientCA was incorrectly used as the master's server cert CA bundle
	// If missing from the config, migrate the ClientCA into that field
	if obj.OAuthConfig != nil && obj.OAuthConfig.MasterCA == nil {
		s := obj.ServingInfo.ClientCA
		// The final value of OAuthConfig.MasterCA should never be nil
		obj.OAuthConfig.MasterCA = &s
	}

	// Ensure no nil plugin config stanzas
	for pluginName := range obj.AdmissionConfig.PluginConfig {
		if obj.AdmissionConfig.PluginConfig[pluginName] == nil {
			obj.AdmissionConfig.PluginConfig[pluginName] = &AdmissionPluginConfig{}
		}
	}

	// Set defaults Cache TTLs for external Webhook Token Reviewers
	for i := range obj.AuthConfig.WebhookTokenAuthenticators {
		if len(obj.AuthConfig.WebhookTokenAuthenticators[i].CacheTTL) == 0 {
			obj.AuthConfig.WebhookTokenAuthenticators[i].CacheTTL = "2m"
		}
	}
}

func SetDefaults_KubernetesMasterConfig(obj *KubernetesMasterConfig) {
	if obj.MasterEndpointReconcileTTL == 0 {
		obj.MasterEndpointReconcileTTL = 15
	}
	if len(obj.APILevels) == 0 {
		obj.APILevels = internal.DefaultKubernetesAPILevels
	}
	if len(obj.ServicesNodePortRange) == 0 {
		obj.ServicesNodePortRange = "30000-32767"
	}
	if len(obj.PodEvictionTimeout) == 0 {
		obj.PodEvictionTimeout = "5m"
	}
}
func SetDefaults_NodeConfig(obj *NodeConfig) {
	if obj.MasterClientConnectionOverrides == nil {
		obj.MasterClientConnectionOverrides = &ClientConnectionOverrides{
			// historical values
			QPS:   10.0,
			Burst: 20,
		}
	}
	SetDefaults_ClientConnectionOverrides(obj.MasterClientConnectionOverrides)

	// Defaults/migrations for NetworkConfig
	if len(obj.NetworkConfig.NetworkPluginName) == 0 {
		obj.NetworkConfig.NetworkPluginName = obj.DeprecatedNetworkPluginName
	}
	if obj.NetworkConfig.MTU == 0 {
		obj.NetworkConfig.MTU = 1450
	}
	if len(obj.IPTablesSyncPeriod) == 0 {
		obj.IPTablesSyncPeriod = "30s"
	}

	// Auth cache defaults
	if len(obj.AuthConfig.AuthenticationCacheTTL) == 0 {
		obj.AuthConfig.AuthenticationCacheTTL = "5m"
	}
	if obj.AuthConfig.AuthenticationCacheSize == 0 {
		obj.AuthConfig.AuthenticationCacheSize = 1000
	}
	if len(obj.AuthConfig.AuthorizationCacheTTL) == 0 {
		obj.AuthConfig.AuthorizationCacheTTL = "5m"
	}
	if obj.AuthConfig.AuthorizationCacheSize == 0 {
		obj.AuthConfig.AuthorizationCacheSize = 1000
	}

	// EnableUnidling by default
	if obj.EnableUnidling == nil {
		v := true
		obj.EnableUnidling = &v
	}
}
func SetDefaults_EtcdStorageConfig(obj *EtcdStorageConfig) {
	if len(obj.KubernetesStorageVersion) == 0 {
		obj.KubernetesStorageVersion = "v1"
	}
	if len(obj.KubernetesStoragePrefix) == 0 {
		obj.KubernetesStoragePrefix = "kubernetes.io"
	}
	if len(obj.OpenShiftStorageVersion) == 0 {
		obj.OpenShiftStorageVersion = internal.DefaultOpenShiftStorageVersionLevel
	}
	if len(obj.OpenShiftStoragePrefix) == 0 {
		obj.OpenShiftStoragePrefix = "openshift.io"
	}
}

func SetDefaults_DockerConfig(obj *DockerConfig) {
	if len(obj.ExecHandlerName) == 0 {
		obj.ExecHandlerName = DockerExecHandlerNative
	}
	if len(obj.DockerShimSocket) == 0 {
		obj.DockerShimSocket = "unix:///var/run/dockershim.sock"
	}
	if len(obj.DockershimRootDirectory) == 0 {
		obj.DockershimRootDirectory = "/var/lib/dockershim"
	}
}

func SetDefaults_ServingInfo(obj *ServingInfo) {
	if len(obj.BindNetwork) == 0 {
		obj.BindNetwork = "tcp4"
	}
}
func SetDefaults_ImagePolicyConfig(obj *ImagePolicyConfig) {
	if obj.MaxImagesBulkImportedPerRepository == 0 {
		obj.MaxImagesBulkImportedPerRepository = 50
	}
	if obj.MaxScheduledImageImportsPerMinute == 0 {
		obj.MaxScheduledImageImportsPerMinute = 60
	}
	if obj.ScheduledImageImportMinimumIntervalSeconds == 0 {
		obj.ScheduledImageImportMinimumIntervalSeconds = 15 * 60
	}
}
func SetDefaults_DNSConfig(obj *DNSConfig) {
	if len(obj.BindNetwork) == 0 {
		obj.BindNetwork = "tcp4"
	}
}
func SetDefaults_SecurityAllocator(obj *SecurityAllocator) {
	if len(obj.UIDAllocatorRange) == 0 {
		obj.UIDAllocatorRange = "1000000000-1999999999/10000"
	}
	if len(obj.MCSAllocatorRange) == 0 {
		obj.MCSAllocatorRange = "s0:/2"
	}
	if obj.MCSLabelsPerProject == 0 {
		obj.MCSLabelsPerProject = 5
	}
}
func SetDefaults_IdentityProvider(obj *IdentityProvider) {
	if len(obj.MappingMethod) == 0 {
		// By default, only let one identity provider authenticate a particular user
		// If multiple identity providers collide, the second one in will fail to auth
		// The admin can set this to "add" if they want to allow new identities to join existing users
		obj.MappingMethod = "claim"
	}
}
func SetDefaults_GrantConfig(obj *GrantConfig) {
	if len(obj.ServiceAccountMethod) == 0 {
		obj.ServiceAccountMethod = "prompt"
	}
}

func addConversionFuncs(scheme *runtime.Scheme) error {
	return scheme.AddConversionFuncs(
		convert_runtime_Object_To_runtime_RawExtension,
		convert_runtime_RawExtension_To_runtime_Object,

		func(in *NodeConfig, out *internal.NodeConfig, s conversion.Scope) error {
			return s.DefaultConvert(in, out, conversion.IgnoreMissingFields)
		},
		func(in *internal.NodeConfig, out *NodeConfig, s conversion.Scope) error {
			return s.DefaultConvert(in, out, conversion.IgnoreMissingFields)
		},
		func(in *KubernetesMasterConfig, out *internal.KubernetesMasterConfig, s conversion.Scope) error {
			if err := s.DefaultConvert(in, out, conversion.IgnoreMissingFields); err != nil {
				return err
			}

			if out.DisabledAPIGroupVersions == nil {
				out.DisabledAPIGroupVersions = map[string][]string{}
			}

			// the APILevels (whitelist) needs to be converted into an internal blacklist
			if len(in.APILevels) == 0 {
				out.DisabledAPIGroupVersions[internal.APIGroupKube] = []string{"*"}

			} else {
				availableLevels := internal.KubeAPIGroupsToAllowedVersions[internal.APIGroupKube]
				whitelistedLevels := sets.NewString(in.APILevels...)
				blacklistedLevels := []string{}

				for _, curr := range availableLevels {
					if !whitelistedLevels.Has(curr) {
						blacklistedLevels = append(blacklistedLevels, curr)
					}
				}

				if len(blacklistedLevels) > 0 {
					out.DisabledAPIGroupVersions[internal.APIGroupKube] = blacklistedLevels
				}
			}

			return nil
		},
		func(in *internal.KubernetesMasterConfig, out *KubernetesMasterConfig, s conversion.Scope) error {
			// internal doesn't have all fields: APILevels
			return s.DefaultConvert(in, out, conversion.IgnoreMissingFields)
		},
		func(in *ServingInfo, out *internal.ServingInfo, s conversion.Scope) error {
			if err := s.DefaultConvert(in, out, conversion.IgnoreMissingFields); err != nil {
				return err
			}
			out.ServerCert.CertFile = in.CertFile
			out.ServerCert.KeyFile = in.KeyFile
			return nil
		},
		func(in *internal.ServingInfo, out *ServingInfo, s conversion.Scope) error {
			if err := s.DefaultConvert(in, out, conversion.IgnoreMissingFields); err != nil {
				return err
			}
			out.CertFile = in.ServerCert.CertFile
			out.KeyFile = in.ServerCert.KeyFile
			return nil
		},
		func(in *RemoteConnectionInfo, out *internal.RemoteConnectionInfo, s conversion.Scope) error {
			out.URL = in.URL
			out.CA = in.CA
			out.ClientCert.CertFile = in.CertFile
			out.ClientCert.KeyFile = in.KeyFile
			return nil
		},
		func(in *internal.RemoteConnectionInfo, out *RemoteConnectionInfo, s conversion.Scope) error {
			out.URL = in.URL
			out.CA = in.CA
			out.CertFile = in.ClientCert.CertFile
			out.KeyFile = in.ClientCert.KeyFile
			return nil
		},
		func(in *EtcdConnectionInfo, out *internal.EtcdConnectionInfo, s conversion.Scope) error {
			out.URLs = in.URLs
			out.CA = in.CA
			out.ClientCert.CertFile = in.CertFile
			out.ClientCert.KeyFile = in.KeyFile
			return nil
		},
		func(in *internal.EtcdConnectionInfo, out *EtcdConnectionInfo, s conversion.Scope) error {
			out.URLs = in.URLs
			out.CA = in.CA
			out.CertFile = in.ClientCert.CertFile
			out.KeyFile = in.ClientCert.KeyFile
			return nil
		},
		func(in *KubeletConnectionInfo, out *internal.KubeletConnectionInfo, s conversion.Scope) error {
			out.Port = in.Port
			out.CA = in.CA
			out.ClientCert.CertFile = in.CertFile
			out.ClientCert.KeyFile = in.KeyFile
			return nil
		},
		func(in *internal.KubeletConnectionInfo, out *KubeletConnectionInfo, s conversion.Scope) error {
			out.Port = in.Port
			out.CA = in.CA
			out.CertFile = in.ClientCert.CertFile
			out.KeyFile = in.ClientCert.KeyFile
			return nil
		},
		func(in *MasterVolumeConfig, out *internal.MasterVolumeConfig, s conversion.Scope) error {
			out.DynamicProvisioningEnabled = (in.DynamicProvisioningEnabled == nil) || (*in.DynamicProvisioningEnabled)
			return nil
		},
		func(in *internal.MasterVolumeConfig, out *MasterVolumeConfig, s conversion.Scope) error {
			enabled := in.DynamicProvisioningEnabled
			out.DynamicProvisioningEnabled = &enabled
			return nil
		},
		func(in *MasterNetworkConfig, out *internal.MasterNetworkConfig, s conversion.Scope) error {
			if err := s.DefaultConvert(in, out, conversion.IgnoreMissingFields); err != nil {
				return err
			}

			if len(out.ClusterNetworks) == 0 {
				out.ClusterNetworks = []internal.ClusterNetworkEntry{
					{
						CIDR:             in.DeprecatedClusterNetworkCIDR,
						HostSubnetLength: in.DeprecatedHostSubnetLength,
					},
				}
			}

			if out.VXLANPort == 0 {
				out.VXLANPort = 4789
			}
			return nil
		},
		func(in *AuditConfig, out *internal.AuditConfig, s conversion.Scope) error {
			if err := s.DefaultConvert(in, out, conversion.IgnoreMissingFields); err != nil {
				return err
			}
			if len(in.AuditFilePath) > 0 {
				out.InternalAuditFilePath = in.AuditFilePath
			}
			return nil
		},
		func(in *internal.AuditConfig, out *AuditConfig, s conversion.Scope) error {
			if err := s.DefaultConvert(in, out, conversion.IgnoreMissingFields); err != nil {
				return err
			}
			return nil
		},

		metav1.Convert_resource_Quantity_To_resource_Quantity,
		metav1.Convert_bool_To_Pointer_bool,
		metav1.Convert_Pointer_bool_To_bool,
	)
}

// convert_runtime_Object_To_runtime_RawExtension attempts to convert runtime.Objects to the appropriate target.
func convert_runtime_Object_To_runtime_RawExtension(in *runtime.Object, out *runtime.RawExtension, s conversion.Scope) error {
	return apihelpers.Convert_runtime_Object_To_runtime_RawExtension(internal.Scheme, in, out, s)
}

// convert_runtime_RawExtension_To_runtime_Object attempts to convert an incoming object into the
// appropriate output type.
func convert_runtime_RawExtension_To_runtime_Object(in *runtime.RawExtension, out *runtime.Object, s conversion.Scope) error {
	return apihelpers.Convert_runtime_RawExtension_To_runtime_Object(internal.Scheme, in, out, s)
}

// SetDefaults_ClientConnectionOverrides defaults a client connection to the pre-1.3 settings of
// being JSON only. Callers must explicitly opt-in to Protobuf support in 1.3+.
func SetDefaults_ClientConnectionOverrides(overrides *ClientConnectionOverrides) {
	if len(overrides.AcceptContentTypes) == 0 {
		overrides.AcceptContentTypes = "application/json"
	}
	if len(overrides.ContentType) == 0 {
		overrides.ContentType = "application/json"
	}
}

var _ runtime.NestedObjectDecoder = &MasterConfig{}

// DecodeNestedObjects handles encoding RawExtensions on the MasterConfig, ensuring the
// objects are decoded with the provided decoder.
func (c *MasterConfig) DecodeNestedObjects(d runtime.Decoder) error {
	// decoding failures result in a runtime.Unknown object being created in Object and passed
	// to conversion
	for k, v := range c.AdmissionConfig.PluginConfig {
		DecodeNestedRawExtensionOrUnknown(d, &v.Configuration)
		c.AdmissionConfig.PluginConfig[k] = v
	}
	if c.OAuthConfig != nil {
		for i := range c.OAuthConfig.IdentityProviders {
			DecodeNestedRawExtensionOrUnknown(d, &c.OAuthConfig.IdentityProviders[i].Provider)
		}
	}
	DecodeNestedRawExtensionOrUnknown(d, &c.AuditConfig.PolicyConfiguration)
	return nil
}

var _ runtime.NestedObjectEncoder = &MasterConfig{}

// EncodeNestedObjects handles encoding RawExtensions on the MasterConfig, ensuring the
// objects are encoded with the provided encoder.
func (c *MasterConfig) EncodeNestedObjects(e runtime.Encoder) error {
	for k, v := range c.AdmissionConfig.PluginConfig {
		if err := EncodeNestedRawExtension(e, &v.Configuration); err != nil {
			return err
		}
		c.AdmissionConfig.PluginConfig[k] = v
	}
	if c.OAuthConfig != nil {
		for i := range c.OAuthConfig.IdentityProviders {
			if err := EncodeNestedRawExtension(e, &c.OAuthConfig.IdentityProviders[i].Provider); err != nil {
				return err
			}
		}
	}
	if err := EncodeNestedRawExtension(e, &c.AuditConfig.PolicyConfiguration); err != nil {
		return err
	}
	return nil
}

// DecodeNestedRawExtensionOrUnknown
func DecodeNestedRawExtensionOrUnknown(d runtime.Decoder, ext *runtime.RawExtension) {
	if ext.Raw == nil || ext.Object != nil {
		return
	}
	obj, gvk, err := d.Decode(ext.Raw, nil, nil)
	if err != nil {
		unk := &runtime.Unknown{Raw: ext.Raw}
		if runtime.IsNotRegisteredError(err) {
			if _, gvk, err := d.Decode(ext.Raw, nil, unk); err == nil {
				unk.APIVersion = gvk.GroupVersion().String()
				unk.Kind = gvk.Kind
				ext.Object = unk
				return
			}
		}
		// TODO: record mime-type with the object
		if gvk != nil {
			unk.APIVersion = gvk.GroupVersion().String()
			unk.Kind = gvk.Kind
		}
		obj = unk
	}
	ext.Object = obj
}

// EncodeNestedRawExtension will encode the object in the RawExtension (if not nil) or
// return an error.
func EncodeNestedRawExtension(e runtime.Encoder, ext *runtime.RawExtension) error {
	if ext.Raw != nil || ext.Object == nil {
		return nil
	}
	data, err := runtime.Encode(e, ext.Object)
	if err != nil {
		return err
	}
	ext.Raw = data
	return nil
}
