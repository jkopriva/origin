package idle

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	utilerrors "github.com/openshift/origin/pkg/util/errors"
	corev1 "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	clientset "k8s.io/client-go/kubernetes"
	kextensionsclient "k8s.io/client-go/kubernetes/typed/extensions/v1beta1"
	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/kubectl/cmd/templates"
	kcmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/kubectl/genericclioptions"
	"k8s.io/kubernetes/pkg/kubectl/genericclioptions/resource"
	"k8s.io/kubernetes/pkg/kubectl/scheme"

	appsv1client "github.com/openshift/client-go/apps/clientset/versioned/typed/apps/v1"
	appsmanualclient "github.com/openshift/origin/pkg/apps/client/v1"
	appsclientinternal "github.com/openshift/origin/pkg/apps/generated/internalclientset"
	"github.com/openshift/origin/pkg/oc/util/ocscheme"
	unidlingapi "github.com/openshift/origin/pkg/unidling/api"
	utilunidling "github.com/openshift/origin/pkg/unidling/util"
	kinternalclientset "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
)

var (
	idleLong = templates.LongDesc(`
		Idle scalable resources

		Idling discovers the scalable resources (such as deployment configs and replication controllers)
		associated with a series of services by examining the endpoints of the service.
		Each service is then marked as idled, the associated resources are recorded, and the resources
		are scaled down to zero replicas.

		Upon receiving network traffic, the services (and any associated routes) will "wake up" the
		associated resources by scaling them back up to their previous scale.`)

	idleExample = templates.Examples(`
		# Idle the scalable controllers associated with the services listed in to-idle.txt
		$ %[1]s idle --resource-names-file to-idle.txt`)
)

type IdleOptions struct {
	dryRun        bool
	filename      string
	all           bool
	selector      string
	allNamespaces bool
	resources     []string

	cmdFullName string

	ClientForMappingFn func(*meta.RESTMapping) (resource.RESTClient, error)
	ClientConfig       *rest.Config
	ClientSet          clientset.Interface
	Mapper             meta.RESTMapper

	// TODO(juanvallejo): remove this once we switch unidling helpers to use external versions
	InternalClientset kinternalclientset.Interface

	Builder   func() *resource.Builder
	Namespace string
	nowTime   time.Time

	genericclioptions.IOStreams
}

func NewIdleOptions(name string, streams genericclioptions.IOStreams) *IdleOptions {
	return &IdleOptions{
		IOStreams:   streams,
		cmdFullName: name,
	}
}

// NewCmdIdle implements the OpenShift cli idle command
func NewCmdIdle(fullName string, f kcmdutil.Factory, streams genericclioptions.IOStreams) *cobra.Command {
	o := NewIdleOptions(fullName, streams)

	cmd := &cobra.Command{
		Use:     "idle (SERVICE_ENDPOINTS... | -l label | --all | --resource-names-file FILENAME)",
		Short:   "Idle scalable resources",
		Long:    idleLong,
		Example: fmt.Sprintf(idleExample, fullName),
		Run: func(cmd *cobra.Command, args []string) {
			kcmdutil.CheckErr(o.Complete(f, cmd, args))
			kcmdutil.CheckErr(o.RunIdle())
		},
	}

	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "If true, only print the annotations that would be written, without annotating or idling the relevant objects")
	cmd.Flags().StringVar(&o.filename, "resource-names-file", o.filename, "file containing list of services whose scalable resources to idle")
	cmd.Flags().StringVarP(&o.selector, "selector", "l", o.selector, "Selector (label query) to use to select services")
	cmd.Flags().BoolVar(&o.all, "all", o.all, "if true, select all services in the namespace")
	cmd.Flags().BoolVar(&o.allNamespaces, "all-namespaces", o.allNamespaces, "if true, select services across all namespaces")
	cmd.MarkFlagFilename("resource-names-file")

	// TODO: take the `-o name` argument, and only print out names instead of the summary

	return cmd
}

func (o *IdleOptions) Complete(f kcmdutil.Factory, cmd *cobra.Command, args []string) error {
	var err error
	o.Namespace, _, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}

	o.nowTime = time.Now().UTC()

	// NB: our filename arg is different from usual, since it's just a list of service names
	if o.filename != "" && (o.selector != "" || len(args) > 0 || o.all) {
		return fmt.Errorf("resource names, selectors, and the all flag may not be be specified if a filename is specified")
	}

	o.ClientConfig, err = f.ToRESTConfig()
	if err != nil {
		return err
	}

	o.ClientSet, err = clientset.NewForConfig(o.ClientConfig)
	if err != nil {
		return err
	}

	o.Mapper, err = f.ToRESTMapper()
	if err != nil {
		return err
	}

	o.InternalClientset, err = f.ClientSet()
	if err != nil {
		return err
	}

	o.ClientForMappingFn = f.ClientForMapping
	o.Builder = f.NewBuilder

	o.resources = args

	return nil
}

// scanLinesFromFile loads lines from either standard in or a file
func scanLinesFromFile(filename string) ([]string, error) {
	var targetsInput io.Reader
	if filename == "-" {
		targetsInput = os.Stdin
	} else if filename == "" {
		return nil, fmt.Errorf("you must specify an list of resources to idle")
	} else {
		inputFile, err := os.Open(filename)
		if err != nil {
			return nil, err
		}
		defer inputFile.Close()
		targetsInput = inputFile
	}

	lines := []string{}

	// grab the raw resources from the file
	lineScanner := bufio.NewScanner(targetsInput)
	for lineScanner.Scan() {
		line := lineScanner.Text()
		if line == "" {
			// skip empty lines
			continue
		}
		lines = append(lines, line)
	}
	if err := lineScanner.Err(); err != nil {
		return nil, err
	}

	return lines, nil
}

// idleUpdateInfo contains the required info to annotate an endpoints object
// with the scalable resources that it should unidle
type idleUpdateInfo struct {
	obj       *corev1.Endpoints
	scaleRefs map[unidlingapi.CrossGroupObjectReference]struct{}
}

// controllerRef contains the small subset of info
// that we need to compare controllers (like ObjectReference,
// or OwnerReference, but with comparable and with just what we need).
type controllerRef struct {
	Name      string
	Namespace string
	Kind      string
	Group     string
}

// calculateIdlableAnnotationsByService calculates the list of objects involved in the idling process from a list of services in a file.
// Using the list of services, it figures out the associated scalable objects, and returns a map from the endpoints object for the services to
// the list of scalable resources associated with that endpoints object, as well as a map from CrossGroupObjectReferences to scale to 0 to the
// name of the associated service.
func (o *IdleOptions) calculateIdlableAnnotationsByService(infoVisitor func(resource.VisitorFunc) error) (map[types.NamespacedName]idleUpdateInfo, map[namespacedCrossGroupObjectReference]types.NamespacedName, error) {
	podsLoaded := make(map[corev1.ObjectReference]*corev1.Pod)
	getPod := func(ref corev1.ObjectReference) (*corev1.Pod, error) {
		if pod, ok := podsLoaded[ref]; ok {
			return pod, nil
		}
		pod, err := o.ClientSet.Core().Pods(ref.Namespace).Get(ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}

		podsLoaded[ref] = pod

		return pod, nil
	}

	controllersLoaded := make(map[namespacedOwnerReference]metav1.Object)
	helpers := make(map[schema.GroupKind]*resource.Helper)
	getController := func(ref namespacedOwnerReference) (metav1.Object, error) {
		if controller, ok := controllersLoaded[ref]; ok {
			return controller, nil
		}
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			return nil, err
		}
		// just get the unversioned version of this
		gk := schema.GroupKind{Group: gv.Group, Kind: ref.Kind}
		helper, ok := helpers[gk]
		if !ok {
			var mapping *meta.RESTMapping
			mapping, err = o.Mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: ref.Kind}, "")
			if err != nil {
				return nil, err
			}
			var client resource.RESTClient
			client, err = o.ClientForMappingFn(mapping)
			if err != nil {
				return nil, err
			}
			helper = resource.NewHelper(client, mapping)
			helpers[gk] = helper
		}

		var controller runtime.Object
		controller, err = helper.Get(ref.namespace, ref.Name, false)
		if err != nil {
			return nil, err
		}

		controllerMeta, err := meta.Accessor(controller)
		if err != nil {
			return nil, err
		}

		controllersLoaded[ref] = controllerMeta

		return controllerMeta, nil
	}

	targetScaleRefs := make(map[namespacedCrossGroupObjectReference]types.NamespacedName)
	endpointsInfo := make(map[types.NamespacedName]idleUpdateInfo)

	err := infoVisitor(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}

		endpoints, isEndpoints := info.Object.(*corev1.Endpoints)
		if !isEndpoints {
			return fmt.Errorf("you must specify endpoints, not %v (view available endpoints with \"%s get endpoints\").", info.Mapping.Resource, o.cmdFullName)
		}

		endpointsName := types.NamespacedName{
			Namespace: endpoints.Namespace,
			Name:      endpoints.Name,
		}
		scaleRefs, err := findScalableResourcesForEndpoints(endpoints, getPod, getController)
		if err != nil {
			return fmt.Errorf("unable to calculate scalable resources for service %s/%s: %v", endpoints.Namespace, endpoints.Name, err)
		}

		nonNamespacedScaleRefs := make(map[unidlingapi.CrossGroupObjectReference]struct{}, len(scaleRefs))

		for ref := range scaleRefs {
			nonNamespacedScaleRefs[ref.CrossGroupObjectReference] = struct{}{}
			targetScaleRefs[ref] = endpointsName
		}

		idleInfo := idleUpdateInfo{
			obj:       endpoints,
			scaleRefs: nonNamespacedScaleRefs,
		}

		endpointsInfo[endpointsName] = idleInfo

		return nil
	})

	return endpointsInfo, targetScaleRefs, err
}

func makeCrossGroupObjRef(ref *metav1.OwnerReference) (unidlingapi.CrossGroupObjectReference, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return unidlingapi.CrossGroupObjectReference{}, err
	}

	return unidlingapi.CrossGroupObjectReference{
		Kind:  ref.Kind,
		Name:  ref.Name,
		Group: gv.Group,
	}, nil
}

// namespacedOwnerReference is an OwnerReference with Namespace info,
// so we differentiate different objects across namespaces.
type namespacedOwnerReference struct {
	metav1.OwnerReference
	namespace string
}

// namespacedCrossGroupObjectReference is a CrossGroupObjectReference
// with namespace information attached, so that we can track relevant
// objects in different namespaces with the same name
type namespacedCrossGroupObjectReference struct {
	unidlingapi.CrossGroupObjectReference
	namespace string
}

// normalizedNSOwnerRef converts an OwnerReference into an namespacedOwnerReference,
// and ensure that it's comparable to other owner references (clearing pointer fields, etc)
func normalizedNSOwnerRef(namespace string, ownerRef *metav1.OwnerReference) namespacedOwnerReference {
	ref := namespacedOwnerReference{
		namespace:      namespace,
		OwnerReference: *ownerRef,
	}

	ref.Controller = nil
	ref.BlockOwnerDeletion = nil

	return ref
}

// findScalableResourcesForEndpoints takes an Endpoints object and looks for the associated
// scalable objects by checking each address in each subset to see if it has a pod
// reference, and the following that pod reference to find the owning controller,
// and returning the unique set of controllers found this way.
func findScalableResourcesForEndpoints(endpoints *corev1.Endpoints, getPod func(corev1.ObjectReference) (*corev1.Pod, error), getController func(namespacedOwnerReference) (metav1.Object, error)) (map[namespacedCrossGroupObjectReference]struct{}, error) {
	// To find all RCs and DCs for an endpoint, we first figure out which pods are pointed to by that endpoint...
	podRefs := map[corev1.ObjectReference]*corev1.Pod{}
	for _, subset := range endpoints.Subsets {
		for _, addr := range subset.Addresses {
			if addr.TargetRef != nil && addr.TargetRef.Kind == "Pod" {
				pod, err := getPod(*addr.TargetRef)
				if utilerrors.TolerateNotFoundError(err) != nil {
					return nil, fmt.Errorf("unable to find controller for pod %s/%s: %v", addr.TargetRef.Namespace, addr.TargetRef.Name, err)
				}

				if pod != nil {
					podRefs[*addr.TargetRef] = pod
				}
			}
		}
	}

	// ... then, for each pod, we check the controller, and find the set of unique controllers...
	immediateControllerRefs := make(map[namespacedOwnerReference]struct{})
	for _, pod := range podRefs {
		controllerRef := metav1.GetControllerOf(pod)
		ref := normalizedNSOwnerRef(pod.Namespace, controllerRef)
		if controllerRef == nil {
			return nil, fmt.Errorf("unable to find controller for pod %s/%s: no creator reference listed", pod.Namespace, pod.Name)
		}
		immediateControllerRefs[ref] = struct{}{}
	}

	// ... finally, for each controller, we load it, and see if there is a corresponding owner (to cover cases like DCs, Deployments, etc)
	controllerRefs := make(map[namespacedCrossGroupObjectReference]struct{})
	for controllerRef := range immediateControllerRefs {
		controller, err := getController(controllerRef)
		if utilerrors.TolerateNotFoundError(err) != nil {
			return nil, fmt.Errorf("unable to load %s %q: %v", controllerRef.Kind, controllerRef.Name, err)
		}

		if controller != nil {
			var parentControllerRef *metav1.OwnerReference
			parentControllerRef = metav1.GetControllerOf(controller)
			var crossGroupObjRef unidlingapi.CrossGroupObjectReference
			if parentControllerRef == nil {
				// if this is just a plain RC, use it
				crossGroupObjRef, err = makeCrossGroupObjRef(&controllerRef.OwnerReference)
			} else {
				crossGroupObjRef, err = makeCrossGroupObjRef(parentControllerRef)
			}

			if err != nil {
				return nil, fmt.Errorf("unable to load the creator of %s %q: %v", controllerRef.Kind, controllerRef.Name, err)
			}
			controllerRefs[namespacedCrossGroupObjectReference{
				CrossGroupObjectReference: crossGroupObjRef,
				namespace:                 controllerRef.namespace,
			}] = struct{}{}
		}
	}

	return controllerRefs, nil
}

// pairScalesWithScaleRefs takes some subresource references, a map of new scales for those subresource references,
// and annotations from an existing object.  It merges the scales and references found in the existing annotations
// with the new data (using the new scale in case of conflict if present and not 0, and the old scale otherwise),
// and returns a slice of RecordedScaleReferences suitable for using as the new annotation value.
func pairScalesWithScaleRefs(serviceName types.NamespacedName, annotations map[string]string, rawScaleRefs map[unidlingapi.CrossGroupObjectReference]struct{}, scales map[namespacedCrossGroupObjectReference]int32) ([]unidlingapi.RecordedScaleReference, error) {
	oldTargetsRaw, hasOldTargets := annotations[unidlingapi.UnidleTargetAnnotation]

	scaleRefs := make([]unidlingapi.RecordedScaleReference, 0, len(rawScaleRefs))

	// initialize the list of new annotations
	for rawScaleRef := range rawScaleRefs {
		scaleRefs = append(scaleRefs, unidlingapi.RecordedScaleReference{
			CrossGroupObjectReference: rawScaleRef,
			Replicas:                  0,
		})
	}

	// if the new preserved scale would be 0, see if we have an old scale that we can use instead
	if hasOldTargets {
		var oldTargets []unidlingapi.RecordedScaleReference
		oldTargetsSet := make(map[unidlingapi.CrossGroupObjectReference]int)
		if err := json.Unmarshal([]byte(oldTargetsRaw), &oldTargets); err != nil {
			return nil, fmt.Errorf("unable to extract existing scale information from endpoints %s: %v", serviceName.String(), err)
		}

		for i, target := range oldTargets {
			oldTargetsSet[target.CrossGroupObjectReference] = i
		}

		// figure out which new targets were already present...
		for _, newScaleRef := range scaleRefs {
			if oldTargetInd, ok := oldTargetsSet[newScaleRef.CrossGroupObjectReference]; ok {
				namespacedScaleRef := namespacedCrossGroupObjectReference{
					CrossGroupObjectReference: newScaleRef.CrossGroupObjectReference,
					namespace:                 serviceName.Namespace,
				}
				if newScale, ok := scales[namespacedScaleRef]; !ok || newScale == 0 {
					scales[namespacedScaleRef] = oldTargets[oldTargetInd].Replicas
				}
				delete(oldTargetsSet, newScaleRef.CrossGroupObjectReference)
			}
		}

		// ...and add in any existing targets not already on the new list to the new list
		for _, ind := range oldTargetsSet {
			scaleRefs = append(scaleRefs, oldTargets[ind])
		}
	}

	for i := range scaleRefs {
		scaleRef := &scaleRefs[i]
		namespacedScaleRef := namespacedCrossGroupObjectReference{
			CrossGroupObjectReference: scaleRef.CrossGroupObjectReference,
			namespace:                 serviceName.Namespace,
		}
		newScale, ok := scales[namespacedScaleRef]
		if !ok || newScale == 0 {
			newScale = 1
			if scaleRef.Replicas != 0 {
				newScale = scaleRef.Replicas
			}
		}

		scaleRef.Replicas = newScale
	}

	return scaleRefs, nil
}

// setIdleAnnotations sets the given annotation on the given object to the marshaled list of CrossGroupObjectReferences
func setIdleAnnotations(serviceName types.NamespacedName, annotations map[string]string, scaleRefs []unidlingapi.RecordedScaleReference, nowTime time.Time) error {
	var scaleRefsBytes []byte
	var err error
	if scaleRefsBytes, err = json.Marshal(scaleRefs); err != nil {
		return err
	}

	annotations[unidlingapi.UnidleTargetAnnotation] = string(scaleRefsBytes)
	annotations[unidlingapi.IdledAtAnnotation] = nowTime.Format(time.RFC3339)

	return nil
}

// patchObj patches calculates a patch between the given new object and the existing marshaled object
func patchObj(obj runtime.Object, metadata metav1.Object, oldData []byte, mapping *meta.RESTMapping, clientForMapping resource.RESTClient) (runtime.Object, error) {
	versionedObj, err := legacyscheme.Scheme.ConvertToVersion(obj, schema.GroupVersions{mapping.GroupVersionKind.GroupVersion()})
	if err != nil {
		return nil, err
	}
	newData, err := json.Marshal(versionedObj)
	if err != nil {
		return nil, err
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, versionedObj)
	if err != nil {
		return nil, err
	}

	helper := resource.NewHelper(clientForMapping, mapping)

	return helper.Patch(metadata.GetNamespace(), metadata.GetName(), types.StrategicMergePatchType, patchBytes)
}

type scaleInfo struct {
	namespace string
	scale     *extensions.Scale
	obj       runtime.Object
}

// RunIdle runs the idling command logic, taking a list of resources or services in a file, scaling the associated
// scalable resources to zero, and annotating the associated endpoints objects with the scalable resources to unidle
// when they receive traffic.
func (o *IdleOptions) RunIdle() error {
	b := o.Builder().
		WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
		ContinueOnError().
		NamespaceParam(o.Namespace).DefaultNamespace().AllNamespaces(o.allNamespaces).
		Flatten().
		SingleResourceType()

	if len(o.filename) > 0 {
		targetServiceNames, err := scanLinesFromFile(o.filename)
		if err != nil {
			return err
		}
		b.ResourceNames("endpoints", targetServiceNames...)
	} else {
		// NB: this is a bit weird because the resource builder will complain if we use ResourceTypes and ResourceNames when len(args) > 0
		if o.selector != "" {
			b.LabelSelectorParam(o.selector).ResourceTypes("endpoints")
		}

		b.ResourceNames("endpoints", o.resources...)

		if o.all {
			b.ResourceTypes("endpoints").SelectAllParam(o.all)
		}
	}

	hadError := false
	nowTime := time.Now().UTC()

	dryRunText := ""
	if o.dryRun {
		dryRunText = "(dry run)"
	}

	// figure out which endpoints and resources we need to idle
	byService, byScalable, err := o.calculateIdlableAnnotationsByService(b.Do().Visit)

	if err != nil {
		if len(byService) == 0 || len(byScalable) == 0 {
			return fmt.Errorf("no valid scalable resources found to idle: %v", err)
		}
		fmt.Fprintf(o.ErrOut, "warning: continuing on for valid scalable resources, but an error occurred while finding scalable resources to idle: %v", err)
	}

	appClient, err := appsclientinternal.NewForConfig(o.ClientConfig)
	if err != nil {
		return err
	}
	appsV1Client, err := appsv1client.NewForConfig(o.ClientConfig)
	if err != nil {
		return err
	}

	externalKubeExtensionClient := kextensionsclient.New(o.ClientSet.Extensions().RESTClient())
	delegScaleGetter := appsmanualclient.NewDelegatingScaleNamespacer(appsV1Client, externalKubeExtensionClient)
	scaleAnnotater := utilunidling.NewScaleAnnotater(delegScaleGetter, appClient.Apps(), o.InternalClientset.Core(), func(currentReplicas int32, annotations map[string]string) {
		annotations[unidlingapi.IdledAtAnnotation] = nowTime.UTC().Format(time.RFC3339)
		annotations[unidlingapi.PreviousScaleAnnotation] = fmt.Sprintf("%v", currentReplicas)
	})

	replicas := make(map[namespacedCrossGroupObjectReference]int32, len(byScalable))
	toScale := make(map[namespacedCrossGroupObjectReference]scaleInfo)

	// first, collect the scale info
	for scaleRef, svcName := range byScalable {
		obj, scale, err := scaleAnnotater.GetObjectWithScale(svcName.Namespace, scaleRef.CrossGroupObjectReference)
		if err != nil {
			fmt.Fprintf(o.ErrOut, "error: unable to get scale for %s %s/%s, not marking that scalable as idled: %v\n", scaleRef.Kind, svcName.Namespace, scaleRef.Name, err)
			svcInfo := byService[svcName]
			delete(svcInfo.scaleRefs, scaleRef.CrossGroupObjectReference)
			hadError = true
			continue
		}
		replicas[scaleRef] = scale.Spec.Replicas
		toScale[scaleRef] = scaleInfo{scale: scale, obj: obj, namespace: svcName.Namespace}
	}

	// annotate the endpoints objects to indicate which scalable resources need to be unidled on traffic
	for serviceName, info := range byService {
		if info.obj.Annotations == nil {
			info.obj.Annotations = make(map[string]string)
		}
		refsWithScale, err := pairScalesWithScaleRefs(serviceName, info.obj.Annotations, info.scaleRefs, replicas)
		if err != nil {
			fmt.Fprintf(o.ErrOut, "error: unable to mark service %q as idled: %v", serviceName.String(), err)
			continue
		}

		if !o.dryRun {
			if len(info.scaleRefs) == 0 {
				fmt.Fprintf(o.ErrOut, "error: unable to mark the service %q as idled.\n", serviceName.String())
				fmt.Fprintf(o.ErrOut, "Make sure that the service is not already marked as idled and that it is associated with resources that can be scaled.\n")
				fmt.Fprintf(o.ErrOut, "See 'oc idle -h' for help and examples.\n")
				hadError = true
				continue
			}

			metadata, err := meta.Accessor(info.obj)
			if err != nil {
				fmt.Fprintf(o.ErrOut, "error: unable to mark service %q as idled: %v", serviceName.String(), err)
				hadError = true
				continue
			}

			gvks, _, err := ocscheme.ReadingInternalScheme.ObjectKinds(info.obj)
			if err != nil {
				fmt.Fprintf(o.ErrOut, "error: unable to mark service %q as idled: %v", serviceName.String(), err)
				hadError = true
				continue
			}
			// we need a versioned obj to properly marshal to JSON, so that we can compute the patch
			mapping, err := o.Mapper.RESTMapping(gvks[0].GroupKind(), gvks[0].Version)
			if err != nil {
				fmt.Fprintf(o.ErrOut, "error: unable to mark service %q as idled: %v", serviceName.String(), err)
				hadError = true
				continue
			}

			versionedObj, err := legacyscheme.Scheme.ConvertToVersion(info.obj, schema.GroupVersions{gvks[0].GroupVersion()})
			if err != nil {
				fmt.Fprintf(o.ErrOut, "error: unable to mark service %q as idled: %v", serviceName.String(), err)
				hadError = true
				continue
			}

			oldData, err := json.Marshal(versionedObj)
			if err != nil {
				fmt.Fprintf(o.ErrOut, "error: unable to mark service %q as idled: %v", serviceName.String(), err)
				hadError = true
				continue
			}

			clientForMapping, err := o.ClientForMappingFn(mapping)

			if err = setIdleAnnotations(serviceName, info.obj.Annotations, refsWithScale, nowTime); err != nil {
				fmt.Fprintf(o.ErrOut, "error: unable to mark service %q as idled: %v", serviceName.String(), err)
				hadError = true
				continue
			}
			if _, err := patchObj(info.obj, metadata, oldData, mapping, clientForMapping); err != nil {
				fmt.Fprintf(o.ErrOut, "error: unable to mark service %q as idled: %v", serviceName.String(), err)
				hadError = true
				continue
			}
		}

		fmt.Fprintf(o.Out, "The service %q has been marked as idled %s\n", serviceName.String(), dryRunText)

		for _, scaleRef := range refsWithScale {
			fmt.Fprintf(o.Out, "The service will unidle %s \"%s/%s\" to %v replicas once it receives traffic %s\n", scaleRef.Kind, serviceName.Namespace, scaleRef.Name, scaleRef.Replicas, dryRunText)
		}
	}

	// actually "idle" the scalable resources by scaling them down to zero
	// (scale down to zero *after* we've applied the annotation so that we don't miss any traffic)
	for scaleRef, info := range toScale {
		if !o.dryRun {
			info.scale.Spec.Replicas = 0
			scaleUpdater := utilunidling.NewScaleUpdater(kcmdutil.InternalVersionJSONEncoder(), info.namespace, appClient.Apps(), o.InternalClientset.Core())
			if err := scaleAnnotater.UpdateObjectScale(scaleUpdater, info.namespace, scaleRef.CrossGroupObjectReference, info.obj, info.scale); err != nil {
				fmt.Fprintf(o.ErrOut, "error: unable to scale %s %s/%s to 0, but still listed as target for unidling: %v\n", scaleRef.Kind, info.namespace, scaleRef.Name, err)
				hadError = true
				continue
			}
		}

		fmt.Fprintf(o.Out, "%s \"%s/%s\" has been idled %s\n", scaleRef.Kind, info.namespace, scaleRef.Name, dryRunText)
	}

	if hadError {
		return kcmdutil.ErrExit
	}

	return nil
}
