package clusterdeployment

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"

	routev1 "github.com/openshift/api/route/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	librarygocontroller "github.com/openshift/library-go/pkg/controller"

	apihelpers "github.com/openshift/hive/apis/helpers"
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	hiveintv1alpha1 "github.com/openshift/hive/apis/hiveinternal/v1alpha1"
	"github.com/openshift/hive/pkg/constants"
	hivemetrics "github.com/openshift/hive/pkg/controller/metrics"
	controllerutils "github.com/openshift/hive/pkg/controller/utils"
	"github.com/openshift/hive/pkg/imageset"
	"github.com/openshift/hive/pkg/install"
	"github.com/openshift/hive/pkg/remoteclient"
	k8slabels "github.com/openshift/hive/pkg/util/labels"
)

// controllerKind contains the schema.GroupVersionKind for this controller type.
var controllerKind = hivev1.SchemeGroupVersion.WithKind("ClusterDeployment")

const (
	ControllerName     = hivev1.ClusterDeploymentControllerName
	defaultRequeueTime = 10 * time.Second
	maxProvisions      = 3

	platformAuthFailureReason = "PlatformAuthError"
	platformAuthSuccessReason = "PlatformAuthSuccess"

	clusterImageSetNotFoundReason = "ClusterImageSetNotFound"
	clusterImageSetFoundReason    = "ClusterImageSetFound"

	defaultDNSNotReadyTimeout     = 10 * time.Minute
	dnsNotReadyReason             = "DNSNotReady"
	dnsNotReadyTimedoutReason     = "DNSNotReadyTimedOut"
	dnsUnsupportedPlatformReason  = "DNSUnsupportedPlatform"
	dnsZoneResourceConflictReason = "DNSZoneResourceConflict"
	dnsReadyReason                = "DNSReady"
	dnsReadyAnnotation            = "hive.openshift.io/dnsready"

	installAttemptsLimitReachedReason = "InstallAttemptsLimitReached"
	installOnlyOnceSetReason          = "InstallOnlyOnceSet"
	provisionNotStoppedReason         = "ProvisionNotStopped"

	deleteAfterAnnotation    = "hive.openshift.io/delete-after" // contains a duration after which the cluster should be cleaned up.
	tryInstallOnceAnnotation = "hive.openshift.io/try-install-once"

	regionUnknown = "unknown"
)

var (
	metricCompletedInstallJobRestarts = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "hive_cluster_deployment_completed_install_restart",
			Help:    "Distribution of the number of restarts for all completed cluster installations.",
			Buckets: []float64{0, 2, 10, 20, 50},
		},
		[]string{"cluster_type"},
	)
	metricInstallJobDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "hive_cluster_deployment_install_job_duration_seconds",
			Help:    "Distribution of the runtime of completed install jobs.",
			Buckets: []float64{60, 300, 600, 1200, 1800, 2400, 3000, 3600},
		},
	)
	metricInstallDelaySeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "hive_cluster_deployment_install_job_delay_seconds",
			Help:    "Time between cluster deployment creation and creation of the job to install/provision the cluster.",
			Buckets: []float64{30, 60, 120, 300, 600, 1200, 1800},
		},
	)
	metricImageSetDelaySeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "hive_cluster_deployment_imageset_job_delay_seconds",
			Help:    "Time between cluster deployment creation and creation of the job which resolves the installer image to use for a ClusterImageSet.",
			Buckets: []float64{10, 30, 60, 300, 600, 1200, 1800},
		},
	)
	metricClustersCreated = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hive_cluster_deployments_created_total",
		Help: "Counter incremented every time we observe a new cluster.",
	},
		[]string{"cluster_type"},
	)
	metricClustersInstalled = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hive_cluster_deployments_installed_total",
		Help: "Counter incremented every time we observe a successful installation.",
	},
		[]string{"cluster_type"},
	)
	metricClustersDeleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hive_cluster_deployments_deleted_total",
		Help: "Counter incremented every time we observe a deleted cluster.",
	},
		[]string{"cluster_type"},
	)
	metricDNSDelaySeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "hive_cluster_deployment_dns_delay_seconds",
			Help:    "Time between cluster deployment with spec.manageDNS creation and the DNSZone becoming ready.",
			Buckets: []float64{10, 30, 60, 300, 600, 1200, 1800},
		},
	)
)

func init() {
	metrics.Registry.MustRegister(metricInstallJobDuration)
	metrics.Registry.MustRegister(metricCompletedInstallJobRestarts)
	metrics.Registry.MustRegister(metricInstallDelaySeconds)
	metrics.Registry.MustRegister(metricImageSetDelaySeconds)
	metrics.Registry.MustRegister(metricClustersCreated)
	metrics.Registry.MustRegister(metricClustersInstalled)
	metrics.Registry.MustRegister(metricClustersDeleted)
	metrics.Registry.MustRegister(metricDNSDelaySeconds)
}

// Add creates a new ClusterDeployment controller and adds it to the manager with default RBAC.
func Add(mgr manager.Manager) error {
	logger := log.WithField("controller", ControllerName)
	concurrentReconciles, clientRateLimiter, queueRateLimiter, err := controllerutils.GetControllerConfig(mgr.GetClient(), ControllerName)
	if err != nil {
		logger.WithError(err).Error("could not get controller configurations")
		return err
	}
	return AddToManager(mgr, NewReconciler(mgr, logger, clientRateLimiter), concurrentReconciles, queueRateLimiter)
}

// NewReconciler returns a new reconcile.Reconciler
func NewReconciler(mgr manager.Manager, logger log.FieldLogger, rateLimiter flowcontrol.RateLimiter) reconcile.Reconciler {
	r := &ReconcileClusterDeployment{
		Client:                                  controllerutils.NewClientWithMetricsOrDie(mgr, ControllerName, &rateLimiter),
		scheme:                                  mgr.GetScheme(),
		logger:                                  logger,
		expectations:                            controllerutils.NewExpectations(logger),
		validateCredentialsForClusterDeployment: controllerutils.ValidateCredentialsForClusterDeployment,
	}
	r.remoteClusterAPIClientBuilder = func(cd *hivev1.ClusterDeployment) remoteclient.Builder {
		return remoteclient.NewBuilder(r.Client, cd, ControllerName)
	}

	protectedDeleteEnvVar := os.Getenv(constants.ProtectedDeleteEnvVar)
	if protectedDelete, err := strconv.ParseBool(protectedDeleteEnvVar); protectedDelete && err == nil {
		logger.Info("Protected Delete enabled")
		r.protectedDelete = true
	}

	return r
}

// AddToManager adds a new Controller to mgr with r as the reconcile.Reconciler
func AddToManager(mgr manager.Manager, r reconcile.Reconciler, concurrentReconciles int, rateLimiter workqueue.RateLimiter) error {
	cdReconciler, ok := r.(*ReconcileClusterDeployment)
	if !ok {
		return errors.New("reconciler supplied is not a ReconcileClusterDeployment")
	}

	c, err := controller.New("clusterdeployment-controller", mgr, controller.Options{
		Reconciler:              r,
		MaxConcurrentReconciles: concurrentReconciles,
		RateLimiter:             rateLimiter,
	})
	if err != nil {
		log.WithField("controller", ControllerName).WithError(err).Error("could not create controller")
		return err
	}

	// Watch for changes to ClusterDeployment
	err = c.Watch(&source.Kind{Type: &hivev1.ClusterDeployment{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		log.WithField("controller", ControllerName).WithError(err).Error("Error watching cluster deployment")
		return err
	}

	// Watch for provisions
	if err := cdReconciler.watchClusterProvisions(c); err != nil {
		return err
	}

	// Watch for jobs created by a ClusterDeployment:
	err = c.Watch(&source.Kind{Type: &batchv1.Job{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &hivev1.ClusterDeployment{},
	})
	if err != nil {
		log.WithField("controller", ControllerName).WithError(err).Error("Error watching cluster deployment job")
		return err
	}

	// Watch for pods created by an install job
	err = c.Watch(&source.Kind{Type: &corev1.Pod{}}, handler.EnqueueRequestsFromMapFunc(selectorPodWatchHandler))
	if err != nil {
		log.WithField("controller", ControllerName).WithError(err).Error("Error watching cluster deployment pods")
		return err
	}

	// Watch for deprovision requests created by a ClusterDeployment
	err = c.Watch(&source.Kind{Type: &hivev1.ClusterDeprovision{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &hivev1.ClusterDeployment{},
	})
	if err != nil {
		log.WithField("controller", ControllerName).WithError(err).Error("Error watching deprovision request created by cluster deployment")
		return err
	}

	// Watch for dnszones created by a ClusterDeployment
	err = c.Watch(&source.Kind{Type: &hivev1.DNSZone{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &hivev1.ClusterDeployment{},
	})
	if err != nil {
		log.WithField("controller", ControllerName).WithError(err).Error("Error watching cluster deployment dnszones")
		return err
	}

	// Watch for changes to ClusterSyncs
	if err := c.Watch(
		&source.Kind{Type: &hiveintv1alpha1.ClusterSync{}},
		&handler.EnqueueRequestForOwner{OwnerType: &hivev1.ClusterDeployment{}},
	); err != nil {
		return errors.Wrap(err, "cannot start watch on ClusterSyncs")
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileClusterDeployment{}

// ReconcileClusterDeployment reconciles a ClusterDeployment object
type ReconcileClusterDeployment struct {
	client.Client
	scheme *runtime.Scheme
	logger log.FieldLogger

	// A TTLCache of clusterprovision creates each clusterdeployment expects to see
	expectations controllerutils.ExpectationsInterface

	// remoteClusterAPIClientBuilder is a function pointer to the function that gets a builder for building a client
	// for the remote cluster's API server
	remoteClusterAPIClientBuilder func(cd *hivev1.ClusterDeployment) remoteclient.Builder

	// validateCredentialsForClusterDeployment is what this controller will call to validate
	// that the platform creds are good (used for testing)
	validateCredentialsForClusterDeployment func(client.Client, *hivev1.ClusterDeployment, log.FieldLogger) (bool, error)

	protectedDelete bool
}

// Reconcile reads that state of the cluster for a ClusterDeployment object and makes changes based on the state read
// and what is in the ClusterDeployment.Spec
func (r *ReconcileClusterDeployment) Reconcile(ctx context.Context, request reconcile.Request) (result reconcile.Result, returnErr error) {
	cdLog := controllerutils.BuildControllerLogger(ControllerName, "clusterDeployment", request.NamespacedName)
	cdLog.Info("reconciling cluster deployment")
	recobsrv := hivemetrics.NewReconcileObserver(ControllerName, cdLog)
	defer recobsrv.ObserveControllerReconcileTime()

	// Fetch the ClusterDeployment instance
	cd := &hivev1.ClusterDeployment{}
	err := r.Get(context.TODO(), request.NamespacedName, cd)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			cdLog.Info("cluster deployment Not Found")
			r.expectations.DeleteExpectations(request.NamespacedName.String())
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		cdLog.WithError(err).Error("Error getting cluster deployment")
		return reconcile.Result{}, err
	}

	// Ensure owner references are correctly set
	err = controllerutils.ReconcileOwnerReferences(cd, generateOwnershipUniqueKeys(cd), r, r.scheme, r.logger)
	if err != nil {
		cdLog.WithError(err).Error("Error reconciling object ownership")
		return reconcile.Result{}, err
	}

	return r.reconcile(request, cd, cdLog)
}

func generateOwnershipUniqueKeys(owner hivev1.MetaRuntimeObject) []*controllerutils.OwnershipUniqueKey {
	return []*controllerutils.OwnershipUniqueKey{
		{
			TypeToList:    &hivev1.ClusterProvisionList{},
			LabelSelector: map[string]string{constants.ClusterDeploymentNameLabel: owner.GetName()},
			Controlled:    true,
		},
		{
			TypeToList: &corev1.PersistentVolumeClaimList{},
			LabelSelector: map[string]string{
				constants.ClusterDeploymentNameLabel: owner.GetName(),
				constants.PVCTypeLabel:               constants.PVCTypeInstallLogs,
			},
			Controlled: true,
		},
		{
			TypeToList: &batchv1.JobList{},
			LabelSelector: map[string]string{
				constants.ClusterDeploymentNameLabel: owner.GetName(),
				constants.JobTypeLabel:               constants.JobTypeImageSet,
			},
			Controlled: true,
		},
		{
			TypeToList: &hivev1.ClusterDeprovisionList{},
			LabelSelector: map[string]string{
				constants.ClusterDeploymentNameLabel: owner.GetName(),
			},
			Controlled: true,
		},
		{
			TypeToList: &hivev1.DNSZoneList{},
			LabelSelector: map[string]string{
				constants.ClusterDeploymentNameLabel: owner.GetName(),
				constants.DNSZoneTypeLabel:           constants.DNSZoneTypeChild,
			},
			Controlled: true,
		},
		{
			TypeToList: &corev1.SecretList{},
			LabelSelector: map[string]string{
				constants.ClusterDeploymentNameLabel: owner.GetName(),
				constants.SecretTypeLabel:            constants.SecretTypeMergedPullSecret,
			},
			Controlled: true,
		},
		{
			TypeToList: &corev1.SecretList{},
			LabelSelector: map[string]string{
				constants.ClusterDeploymentNameLabel: owner.GetName(),
				constants.SecretTypeLabel:            constants.SecretTypeKubeConfig,
			},
			Controlled: false,
		},
		{
			TypeToList: &corev1.SecretList{},
			LabelSelector: map[string]string{
				constants.ClusterDeploymentNameLabel: owner.GetName(),
				constants.SecretTypeLabel:            constants.SecretTypeKubeAdminCreds,
			},
			Controlled: false,
		},
	}
}

func (r *ReconcileClusterDeployment) addAdditionalKubeconfigCAs(cd *hivev1.ClusterDeployment,
	cdLog log.FieldLogger) error {

	adminKubeconfigSecret := &corev1.Secret{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: cd.Namespace, Name: cd.Spec.ClusterMetadata.AdminKubeconfigSecretRef.Name}, adminKubeconfigSecret); err != nil {
		cdLog.WithError(err).Error("failed to get admin kubeconfig secret")
		return err
	}

	originalSecret := adminKubeconfigSecret.DeepCopy()

	rawData, hasRawData := adminKubeconfigSecret.Data[constants.RawKubeconfigSecretKey]
	if !hasRawData {
		adminKubeconfigSecret.Data[constants.RawKubeconfigSecretKey] = adminKubeconfigSecret.Data[constants.KubeconfigSecretKey]
		rawData = adminKubeconfigSecret.Data[constants.KubeconfigSecretKey]
	}

	var err error
	adminKubeconfigSecret.Data[constants.KubeconfigSecretKey], err = controllerutils.AddAdditionalKubeconfigCAs(rawData)
	if err != nil {
		cdLog.WithError(err).Errorf("error adding additional CAs to admin kubeconfig")
		return err
	}

	if reflect.DeepEqual(originalSecret.Data, adminKubeconfigSecret.Data) {
		cdLog.Debug("secret data has not changed, no need to update")
		return nil
	}

	cdLog.Info("admin kubeconfig has been modified, updating")
	err = r.Update(context.TODO(), adminKubeconfigSecret)
	if err != nil {
		cdLog.WithError(err).Log(controllerutils.LogLevel(err), "error updating admin kubeconfig secret")
		return err
	}

	return nil
}

func (r *ReconcileClusterDeployment) reconcile(request reconcile.Request, cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) (result reconcile.Result, returnErr error) {
	// Set platform label on the ClusterDeployment
	if platform := getClusterPlatform(cd); cd.Labels[hivev1.HiveClusterPlatformLabel] != platform {
		if cd.Labels == nil {
			cd.Labels = make(map[string]string)
		}
		if cd.Labels[hivev1.HiveClusterPlatformLabel] != "" {
			cdLog.Warnf("changing the value of %s from %s to %s", hivev1.HiveClusterPlatformLabel,
				cd.Labels[hivev1.HiveClusterPlatformLabel], platform)
		}
		cd.Labels[hivev1.HiveClusterPlatformLabel] = platform
		err := r.Update(context.TODO(), cd)
		if err != nil {
			cdLog.WithError(err).Log(controllerutils.LogLevel(err), "failed to set cluster platform label")
		}
		return reconcile.Result{}, err
	}

	// Set region label on the ClusterDeployment
	if region := getClusterRegion(cd); cd.Spec.Platform.BareMetal == nil && cd.Spec.Platform.AgentBareMetal == nil &&
		cd.Labels[hivev1.HiveClusterRegionLabel] != region {

		if cd.Labels == nil {
			cd.Labels = make(map[string]string)
		}
		if cd.Labels[hivev1.HiveClusterRegionLabel] != "" {
			cdLog.Warnf("changing the value of %s from %s to %s", hivev1.HiveClusterRegionLabel,
				cd.Labels[hivev1.HiveClusterRegionLabel], region)
		}
		cd.Labels[hivev1.HiveClusterRegionLabel] = region
		err := r.Update(context.TODO(), cd)
		if err != nil {
			cdLog.WithError(err).Log(controllerutils.LogLevel(err), "failed to set cluster region label")
		}
		return reconcile.Result{}, err
	}

	if cd.DeletionTimestamp != nil {
		if !controllerutils.HasFinalizer(cd, hivev1.FinalizerDeprovision) {
			// Make sure we have no deprovision underway metric even though this was probably cleared when we
			// removed the finalizer.
			clearDeprovisionUnderwaySecondsMetric(cd, cdLog)

			return reconcile.Result{}, nil
		}

		// Deprovision still underway, report metric for this cluster.
		hivemetrics.MetricClusterDeploymentDeprovisioningUnderwaySeconds.WithLabelValues(
			cd.Name,
			cd.Namespace,
			hivemetrics.GetClusterDeploymentType(cd)).Set(
			time.Since(cd.DeletionTimestamp.Time).Seconds())

		return r.syncDeletedClusterDeployment(cd, cdLog)
	}

	// Check for the delete-after annotation, and if the cluster has expired, delete it
	deleteAfter, ok := cd.Annotations[deleteAfterAnnotation]
	if ok {
		cdLog.Debugf("found delete after annotation: %s", deleteAfter)
		dur, err := time.ParseDuration(deleteAfter)
		if err != nil {
			cdLog.WithError(err).WithField("deleteAfter", deleteAfter).Infof("error parsing %s as a duration", deleteAfterAnnotation)
			return reconcile.Result{}, fmt.Errorf("error parsing %s as a duration: %v", deleteAfterAnnotation, err)
		}
		if !cd.CreationTimestamp.IsZero() {
			expiry := cd.CreationTimestamp.Add(dur)
			cdLog.Debugf("cluster expires at: %s", expiry)
			if time.Now().After(expiry) {
				cdLog.WithField("expiry", expiry).Info("cluster has expired, issuing delete")
				err := r.Delete(context.TODO(), cd)
				if err != nil {
					cdLog.WithError(err).Log(controllerutils.LogLevel(err), "error deleting expired cluster")
				}
				return reconcile.Result{}, err
			}

			defer func() {
				// We have an expiry time but we're not expired yet. Set requeueAfter to the expiry time
				// so that we requeue cluster for deletion once reconcile has completed
				result, returnErr = controllerutils.EnsureRequeueAtLeastWithin(
					time.Until(cd.CreationTimestamp.Add(dur)),
					result,
					returnErr,
				)
			}()

		}
	}

	if !controllerutils.HasFinalizer(cd, hivev1.FinalizerDeprovision) {
		cdLog.Debugf("adding clusterdeployment finalizer")
		if err := r.addClusterDeploymentFinalizer(cd); err != nil {
			cdLog.WithError(err).Log(controllerutils.LogLevel(err), "error adding finalizer")
			return reconcile.Result{}, err
		}
		metricClustersCreated.WithLabelValues(hivemetrics.GetClusterDeploymentType(cd)).Inc()
		return reconcile.Result{}, nil
	}

	if cd.Spec.Installed {
		// set installedTimestamp for adopted clusters
		if cd.Status.InstalledTimestamp == nil {
			cd.Status.InstalledTimestamp = &cd.ObjectMeta.CreationTimestamp
			if err := r.Status().Update(context.TODO(), cd); err != nil {
				cdLog.WithError(err).Log(controllerutils.LogLevel(err), "could not set cluster installed timestamp")
				return reconcile.Result{Requeue: true}, nil
			}
		}

		// update SyncSetFailedCondition status condition
		cdLog.Debug("Check if any syncsets are failing")
		if err := r.setSyncSetFailedCondition(cd, cdLog); err != nil {
			cdLog.WithError(err).Error("Error updating SyncSetFailedCondition status condition")
			return reconcile.Result{}, err
		}

		// delete failed provisions which are more than 7 days old
		existingProvisions, err := r.existingProvisions(cd, cdLog)
		if err != nil {
			return reconcile.Result{}, err
		}
		r.deleteOldFailedProvisions(existingProvisions, cdLog)

		cdLog.Debug("cluster is already installed, no processing of provision needed")
		r.cleanupInstallLogPVC(cd, cdLog)

		if cd.Spec.ClusterMetadata != nil &&
			cd.Spec.ClusterMetadata.AdminKubeconfigSecretRef.Name != "" {

			if err := r.addAdditionalKubeconfigCAs(cd, cdLog); err != nil {
				return reconcile.Result{}, err
			}

			// Add cluster deployment as additional owner reference to admin secrets
			if err := r.addOwnershipToSecret(cd, cdLog, cd.Spec.ClusterMetadata.AdminKubeconfigSecretRef.Name); err != nil {
				return reconcile.Result{}, err
			}
			if cd.Spec.ClusterMetadata.AdminPasswordSecretRef.Name != "" {
				if err := r.addOwnershipToSecret(cd, cdLog, cd.Spec.ClusterMetadata.AdminPasswordSecretRef.Name); err != nil {
					return reconcile.Result{}, err
				}
			}

			if cd.Status.WebConsoleURL == "" || cd.Status.APIURL == "" {
				return r.setClusterStatusURLs(cd, cdLog)
			}

		}
		return reconcile.Result{}, nil
	}

	// If the ClusterDeployment is being relocated to another Hive instance, stop any current provisioning and do not
	// do any more reconciling.
	switch _, relocateStatus, err := controllerutils.IsRelocating(cd); {
	case err != nil:
		return reconcile.Result{}, errors.Wrap(err, "could not determine relocate status")
	case relocateStatus == hivev1.RelocateOutgoing:
		result, err := r.stopProvisioning(cd, cdLog)
		if result == nil {
			result = &reconcile.Result{}
		}
		return *result, err
	}

	// Sanity check the platform/cloud credentials.
	validCreds, err := r.validatePlatformCreds(cd, cdLog)
	if err != nil {
		cdLog.WithError(err).Error("unable to validate platform credentials")
		return reconcile.Result{}, err
	}
	// Make sure the condition is set properly.
	_, err = r.setAuthenticationFailure(cd, validCreds, cdLog)
	if err != nil {
		cdLog.WithError(err).Error("unable to update clusterdeployment")
		return reconcile.Result{}, err
	}

	// If the platform credentials are no good, return error and go into backoff
	authCondition := controllerutils.FindClusterDeploymentCondition(cd.Status.Conditions, hivev1.AuthenticationFailureClusterDeploymentCondition)
	if authCondition != nil && authCondition.Status == corev1.ConditionTrue {
		authError := errors.New(authCondition.Message)
		cdLog.WithError(authError).Error("cannot proceed with provision while platform credentials authentication is failing.")
		return reconcile.Result{}, authError
	}

	imageSet, err := r.getClusterImageSet(cd, cdLog)
	if err != nil {
		return reconcile.Result{}, err
	}

	releaseImage := r.getReleaseImage(cd, imageSet, cdLog)

	cdLog.Debug("loading pull secrets")
	pullSecret, err := r.mergePullSecrets(cd, cdLog)
	if err != nil {
		cdLog.WithError(err).Error("Error merging pull secrets")
		return reconcile.Result{}, err
	}

	// Update the pull secret object if required
	switch updated, err := r.updatePullSecretInfo(pullSecret, cd, cdLog); {
	case err != nil:
		cdLog.WithError(err).Log(controllerutils.LogLevel(err), "Error updating the merged pull secret")
		return reconcile.Result{}, err
	case updated:
		// The controller will not automatically requeue the cluster deployment
		// since the controller is not watching for secrets. So, requeue manually.
		return reconcile.Result{Requeue: true}, nil
	}

	switch result, err := r.resolveInstallerImage(cd, imageSet, releaseImage, cdLog); {
	case err != nil:
		return reconcile.Result{}, err
	case result != nil:
		return *result, nil
	}

	if !r.expectations.SatisfiedExpectations(request.String()) {
		cdLog.Debug("waiting for expectations to be satisfied")
		return reconcile.Result{}, nil
	}

	// Return early and stop processing if Agent install strategy is in play. The controllers that
	// handle this portion of the API currently live in the assisted service repo, rather than hive.
	// This will hopefully change in the future.
	if cd.Spec.Provisioning != nil && cd.Spec.Provisioning.InstallStrategy != nil && cd.Spec.Provisioning.InstallStrategy.Agent != nil {
		cdLog.Info("skipping processing of agent install strategy cluster")
		return reconcile.Result{}, nil
	}

	if cd.Status.ProvisionRef == nil {
		return r.startNewProvision(cd, releaseImage, cdLog)
	}

	return r.reconcileExistingProvision(cd, cdLog)
}

func isDNSNotReadyConditionSet(cd *hivev1.ClusterDeployment) (bool, *hivev1.ClusterDeploymentCondition) {
	dnsNotReadyCondition := controllerutils.FindClusterDeploymentCondition(cd.Status.Conditions, hivev1.DNSNotReadyCondition)
	return dnsNotReadyCondition != nil &&
			dnsNotReadyCondition.Status == corev1.ConditionTrue &&
			(dnsNotReadyCondition.Reason == dnsNotReadyReason || dnsNotReadyCondition.Reason == dnsNotReadyTimedoutReason),
		dnsNotReadyCondition
}

func (r *ReconcileClusterDeployment) startNewProvision(
	cd *hivev1.ClusterDeployment,
	releaseImage string,
	cdLog log.FieldLogger,
) (result reconcile.Result, returnedErr error) {
	existingProvisions, err := r.existingProvisions(cd, cdLog)
	if err != nil {
		return reconcile.Result{}, err
	}

	for _, provision := range existingProvisions {
		if provision.Spec.Stage != hivev1.ClusterProvisionStageFailed {
			return reconcile.Result{}, r.adoptProvision(cd, provision, cdLog)
		}
	}

	r.deleteStaleProvisions(existingProvisions, cdLog)

	if cd.Spec.ManageDNS {
		dnsZone, err := r.ensureManagedDNSZone(cd, cdLog)
		if err != nil {
			return reconcile.Result{}, err
		}
		if dnsZone == nil {
			// dnsNotReady condition was set.
			if isSet, _ := isDNSNotReadyConditionSet(cd); isSet {
				// dnsNotReadyReason is why the dnsNotReady condition was set, therefore requeue so that we check to see if it times out.
				// add defaultRequeueTime to avoid the race condition where the controller is reconciled at the exact time of the timeout (unlikely, but possible).
				return reconcile.Result{RequeueAfter: defaultDNSNotReadyTimeout + defaultRequeueTime}, nil
			}

			return reconcile.Result{}, nil
		}

		updated, err := r.setDNSDelayMetric(cd, dnsZone, cdLog)
		if updated || err != nil {
			return reconcile.Result{}, err
		}
	}

	if cd.Status.InstallRestarts > 0 && cd.Annotations[tryInstallOnceAnnotation] == "true" {
		cdLog.Debug("not creating new provision since the deployment is set to try install only once")
		conditions, changed := controllerutils.SetClusterDeploymentConditionWithChangeCheck(
			cd.Status.Conditions,
			hivev1.ProvisionStoppedCondition,
			corev1.ConditionTrue,
			installOnlyOnceSetReason,
			"Deployment is set to try install only once",
			controllerutils.UpdateConditionIfReasonOrMessageChange)
		if changed {
			cd.Status.Conditions = conditions
			cdLog.Debugf("setting ProvisionStoppedCondition to %v", corev1.ConditionTrue)
			if err := r.Status().Update(context.TODO(), cd); err != nil {
				cdLog.WithError(err).Log(controllerutils.LogLevel(err), "failed to update cluster deployment status")
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}
	if cd.Spec.InstallAttemptsLimit != nil && cd.Status.InstallRestarts >= int(*cd.Spec.InstallAttemptsLimit) {
		cdLog.Debug("not creating new provision since the install attempts limit has been reached")
		conditions, changed := controllerutils.SetClusterDeploymentConditionWithChangeCheck(
			cd.Status.Conditions,
			hivev1.ProvisionStoppedCondition,
			corev1.ConditionTrue,
			installAttemptsLimitReachedReason,
			"Install attempts limit reached",
			controllerutils.UpdateConditionIfReasonOrMessageChange)
		if changed {
			cd.Status.Conditions = conditions
			cdLog.Debugf("setting ProvisionStoppedCondition to %v", corev1.ConditionTrue)
			if err := r.Status().Update(context.TODO(), cd); err != nil {
				cdLog.WithError(err).Log(controllerutils.LogLevel(err), "failed to update cluster deployment status")
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}

	conditions, changed := controllerutils.SetClusterDeploymentConditionWithChangeCheck(
		cd.Status.Conditions,
		hivev1.ProvisionStoppedCondition,
		corev1.ConditionFalse,
		provisionNotStoppedReason,
		"Provision is not stopped",
		controllerutils.UpdateConditionNever)
	if changed {
		cd.Status.Conditions = conditions
		cdLog.Debugf("setting ProvisionStoppedCondition to %v", corev1.ConditionFalse)
		if err := r.Status().Update(context.TODO(), cd); err != nil {
			cdLog.WithError(err).Log(controllerutils.LogLevel(err), "failed to update cluster deployment status")
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}

	if err := controllerutils.SetupClusterInstallServiceAccount(r, cd.Namespace, cdLog); err != nil {
		cdLog.WithError(err).Log(controllerutils.LogLevel(err), "error setting up service account and role")
		return reconcile.Result{}, err
	}

	provisionName := apihelpers.GetResourceName(cd.Name, fmt.Sprintf("%d-%s", cd.Status.InstallRestarts, utilrand.String(5)))

	labels := cd.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	labels[constants.ClusterDeploymentNameLabel] = cd.Name

	extraEnvVars := getInstallLogEnvVars(cd.Name)
	extraEnvVars = append(extraEnvVars, getAWSServiceProviderEnvVars(cd, cd.Name)...)

	podSpec, err := install.InstallerPodSpec(
		cd,
		provisionName,
		releaseImage,
		controllerutils.InstallServiceAccountName,
		os.Getenv("HTTP_PROXY"),
		os.Getenv("HTTPS_PROXY"),
		os.Getenv("NO_PROXY"),
		extraEnvVars,
	)
	if err != nil {
		cdLog.WithError(err).Error("could not generate installer pod spec")
		return reconcile.Result{}, err
	}

	provision := &hivev1.ClusterProvision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      provisionName,
			Namespace: cd.Namespace,
			Labels:    labels,
		},
		Spec: hivev1.ClusterProvisionSpec{
			ClusterDeploymentRef: corev1.LocalObjectReference{
				Name: cd.Name,
			},
			PodSpec: *podSpec,
			Attempt: cd.Status.InstallRestarts,
			Stage:   hivev1.ClusterProvisionStageInitializing,
		},
	}

	// Copy over the cluster ID and infra ID from previous provision so that a failed install can be removed.
	if cd.Spec.ClusterMetadata != nil {
		provision.Spec.PrevClusterID = &cd.Spec.ClusterMetadata.ClusterID
		provision.Spec.PrevInfraID = &cd.Spec.ClusterMetadata.InfraID
	}

	cdLog.WithField("derivedObject", provision.Name).Debug("Setting label on derived object")
	provision.Labels = k8slabels.AddLabel(provision.Labels, constants.ClusterDeploymentNameLabel, cd.Name)
	if err := controllerutil.SetControllerReference(cd, provision, r.scheme); err != nil {
		cdLog.WithError(err).Error("could not set the owner ref on provision")
		return reconcile.Result{}, err
	}

	if err := r.copyInstallLogSecret(provision.Namespace, extraEnvVars); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			// Couldn't copy the install log secret for a reason other than it already exists.
			// If the secret already exists, then we should just use that secret.
			cdLog.WithError(err).Error("could not copy install log secret")
			return reconcile.Result{}, err
		}
	}

	if err := install.CopyAWSServiceProviderSecret(r.Client, provision.Namespace, extraEnvVars, cd, r.scheme); err != nil {
		cdLog.WithError(err).Error("could not copy AWS service provider secret")
		return reconcile.Result{}, err
	}

	if err := r.setupAWSCredentialForAssumeRole(cd); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			// Couldn't create the assume role credential secret for a reason other than it already exists.
			// If the secret already exists, then we should just use that secret.
			cdLog.WithError(err).Error("could not create AWS assume role credential secret")
			return reconcile.Result{}, err
		}
	}

	r.expectations.ExpectCreations(types.NamespacedName{Namespace: cd.Namespace, Name: cd.Name}.String(), 1)
	if err := r.Create(context.TODO(), provision); err != nil {
		cdLog.WithError(err).Error("could not create provision")
		r.expectations.CreationObserved(types.NamespacedName{Namespace: cd.Namespace, Name: cd.Name}.String())
		return reconcile.Result{}, err
	}

	cdLog.WithField("provision", provision.Name).Info("created new provision")

	if cd.Status.InstallRestarts == 0 {
		kickstartDuration := time.Since(cd.CreationTimestamp.Time)
		cdLog.WithField("elapsed", kickstartDuration.Seconds()).Info("calculated time to first provision seconds")
		metricInstallDelaySeconds.Observe(float64(kickstartDuration.Seconds()))
	}

	return reconcile.Result{}, nil
}

func (r *ReconcileClusterDeployment) copyInstallLogSecret(destNamespace string, extraEnvVars []corev1.EnvVar) error {
	hiveNS := controllerutils.GetHiveNamespace()

	srcSecretName, foundSrc := os.LookupEnv(constants.InstallLogsCredentialsSecretRefEnvVar)
	if !foundSrc {
		// If the src secret reference wasn't found, then don't attempt to copy the secret.
		return nil
	}

	foundDest := false
	var destSecretName string
	for _, envVar := range extraEnvVars {
		if envVar.Name == constants.InstallLogsCredentialsSecretRefEnvVar {
			destSecretName = envVar.Value
			foundDest = true
		}
	}

	if !foundDest {
		// If the dest secret reference wasn't found, then don't attempt to copy the secret.
		return nil
	}

	src := types.NamespacedName{Name: srcSecretName, Namespace: hiveNS}
	dest := types.NamespacedName{Name: destSecretName, Namespace: destNamespace}
	return controllerutils.CopySecret(r, src, dest, nil, nil)
}

func getInstallLogEnvVars(secretPrefix string) []corev1.EnvVar {
	extraEnvVars := []corev1.EnvVar{}

	cloudProvider, found := os.LookupEnv(constants.InstallLogsUploadProviderEnvVar)
	if !found {
		return extraEnvVars
	}

	extraEnvVars = append(extraEnvVars, corev1.EnvVar{
		Name:  constants.InstallLogsUploadProviderEnvVar,
		Value: cloudProvider,
	})

	if cloudProvider == constants.InstallLogsUploadProviderAWS {
		secretName, foundSrc := os.LookupEnv(constants.InstallLogsCredentialsSecretRefEnvVar)
		if foundSrc {
			extraEnvVars = append(extraEnvVars, corev1.EnvVar{
				Name:  constants.InstallLogsCredentialsSecretRefEnvVar,
				Value: secretPrefix + "-" + secretName,
			})
		}

		extraEnvVars = addEnvVarIfFound(constants.InstallLogsAWSRegionEnvVar, extraEnvVars)
		extraEnvVars = addEnvVarIfFound(constants.InstallLogsAWSServiceEndpointEnvVar, extraEnvVars)
		extraEnvVars = addEnvVarIfFound(constants.InstallLogsAWSS3BucketEnvVar, extraEnvVars)
	}

	return extraEnvVars
}

func getAWSServiceProviderEnvVars(cd *hivev1.ClusterDeployment, secretPrefix string) []corev1.EnvVar {
	var extraEnvVars []corev1.EnvVar
	spSecretName := os.Getenv(constants.HiveAWSServiceProviderCredentialsSecretRefEnvVar)
	if spSecretName == "" {
		return extraEnvVars
	}

	if cd.Spec.Platform.AWS == nil {
		return extraEnvVars
	}

	extraEnvVars = append(extraEnvVars, corev1.EnvVar{
		Name:  constants.HiveAWSServiceProviderCredentialsSecretRefEnvVar,
		Value: secretPrefix + "-" + spSecretName,
	})
	return extraEnvVars
}

func (r *ReconcileClusterDeployment) setupAWSCredentialForAssumeRole(cd *hivev1.ClusterDeployment) error {
	if cd.Spec.Platform.AWS == nil ||
		cd.Spec.Platform.AWS.CredentialsSecretRef.Name != "" ||
		cd.Spec.Platform.AWS.CredentialsAssumeRole == nil {
		// no setup required
		return nil
	}

	return install.AWSAssumeRoleCLIConfig(r.Client, cd.Spec.Platform.AWS.CredentialsAssumeRole, install.AWSAssumeRoleSecretName(cd.Name), cd.Namespace, cd, r.scheme)
}

func addEnvVarIfFound(name string, envVars []corev1.EnvVar) []corev1.EnvVar {
	value, found := os.LookupEnv(name)
	if !found {
		return envVars
	}

	tmp := corev1.EnvVar{
		Name:  name,
		Value: value,
	}

	return append(envVars, tmp)
}

func (r *ReconcileClusterDeployment) reconcileExistingProvision(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) (result reconcile.Result, returnedErr error) {
	cdLog = cdLog.WithField("provision", cd.Status.ProvisionRef.Name)
	cdLog.Debug("reconciling existing provision")

	provision := &hivev1.ClusterProvision{}
	switch err := r.Get(context.TODO(), types.NamespacedName{Name: cd.Status.ProvisionRef.Name, Namespace: cd.Namespace}, provision); {
	case apierrors.IsNotFound(err):
		cdLog.Warn("linked provision not found")
		return r.clearOutCurrentProvision(cd, cdLog)
	case err != nil:
		cdLog.WithError(err).Error("could not get provision")
		return reconcile.Result{}, err
	}

	// Save the cluster ID and infra ID from the provision so that we can
	// clean up partial installs on the next provision attempt in case of failure.
	if provision.Spec.InfraID != nil {
		clusterMetadata := &hivev1.ClusterMetadata{}
		clusterMetadata.InfraID = *provision.Spec.InfraID
		if provision.Spec.ClusterID != nil {
			clusterMetadata.ClusterID = *provision.Spec.ClusterID
		}
		if provision.Spec.AdminKubeconfigSecretRef != nil {
			clusterMetadata.AdminKubeconfigSecretRef = *provision.Spec.AdminKubeconfigSecretRef
		}
		if provision.Spec.AdminPasswordSecretRef != nil {
			clusterMetadata.AdminPasswordSecretRef = *provision.Spec.AdminPasswordSecretRef
		}
		if !reflect.DeepEqual(clusterMetadata, cd.Spec.ClusterMetadata) {
			cd.Spec.ClusterMetadata = clusterMetadata
			cdLog.Infof("Saving infra ID %q for cluster", cd.Spec.ClusterMetadata.InfraID)
			err := r.Update(context.TODO(), cd)
			if err != nil {
				cdLog.WithError(err).Log(controllerutils.LogLevel(err), "error updating clusterdeployment status with infra ID")
			}
			return reconcile.Result{}, err
		}
	}

	switch provision.Spec.Stage {
	case hivev1.ClusterProvisionStageInitializing:
		return r.reconcileInitializingProvision(cd, provision, cdLog)
	case hivev1.ClusterProvisionStageProvisioning:
		return r.reconcileProvisioningProvision(cd, provision, cdLog)
	case hivev1.ClusterProvisionStageFailed:
		return r.reconcileFailedProvision(cd, provision, cdLog)
	case hivev1.ClusterProvisionStageComplete:
		return r.reconcileCompletedProvision(cd, provision, cdLog)
	default:
		cdLog.WithField("stage", provision.Spec.Stage).Error("unknown provision stage")
		return reconcile.Result{}, errors.New("unknown provision stage")
	}
}

func (r *ReconcileClusterDeployment) reconcileInitializingProvision(cd *hivev1.ClusterDeployment, provision *hivev1.ClusterProvision, cdLog log.FieldLogger) (reconcile.Result, error) {
	cdLog.Debug("still initializing provision")
	// Set condition on ClusterDeployment when install pod is stuck in pending phase
	installPodStuckCondition := controllerutils.FindClusterProvisionCondition(provision.Status.Conditions, hivev1.InstallPodStuckCondition)
	if installPodStuckCondition != nil && installPodStuckCondition.Status == corev1.ConditionTrue {
		if err := r.setInstallLaunchErrorCondition(cd, corev1.ConditionTrue, installPodStuckCondition.Reason, installPodStuckCondition.Message, cdLog); err != nil {
			cdLog.WithError(err).Log(controllerutils.LogLevel(err), "could not update InstallLaunchErrorCondition")
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}

func (r *ReconcileClusterDeployment) reconcileProvisioningProvision(cd *hivev1.ClusterDeployment, provision *hivev1.ClusterProvision, cdLog log.FieldLogger) (reconcile.Result, error) {
	cdLog.Debug("still provisioning")
	if err := r.setInstallLaunchErrorCondition(cd, corev1.ConditionFalse, "InstallLaunchSuccessful", "Successfully launched install pod", cdLog); err != nil {
		cdLog.WithError(err).Log(controllerutils.LogLevel(err), "could not update InstallLaunchErrorCondition")
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (r *ReconcileClusterDeployment) reconcileFailedProvision(cd *hivev1.ClusterDeployment, provision *hivev1.ClusterProvision, cdLog log.FieldLogger) (reconcile.Result, error) {
	nextProvisionTime := time.Now()
	reason := "MissingCondition"

	failedCond := controllerutils.FindClusterProvisionCondition(provision.Status.Conditions, hivev1.ClusterProvisionFailedCondition)
	if failedCond != nil && failedCond.Status == corev1.ConditionTrue {
		nextProvisionTime = calculateNextProvisionTime(failedCond.LastTransitionTime.Time, cd.Status.InstallRestarts, cdLog)
		reason = failedCond.Reason
	} else {
		cdLog.Warnf("failed provision does not have a %s condition", hivev1.ClusterProvisionFailedCondition)
	}

	newConditions, condChange := controllerutils.SetClusterDeploymentConditionWithChangeCheck(
		cd.Status.Conditions,
		hivev1.ProvisionFailedCondition,
		corev1.ConditionTrue,
		reason,
		fmt.Sprintf("Provision %s failed. Next provision at %s.", provision.Name, nextProvisionTime.UTC().Format(time.RFC3339)),
		controllerutils.UpdateConditionIfReasonOrMessageChange,
	)
	cd.Status.Conditions = newConditions

	timeUntilNextProvision := time.Until(nextProvisionTime)
	if timeUntilNextProvision.Seconds() > 0 {
		cdLog.WithField("nextProvision", nextProvisionTime).Info("waiting to start a new provision after failure")
		if condChange {
			if err := r.statusUpdate(cd, cdLog); err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{RequeueAfter: timeUntilNextProvision}, nil
	}

	cdLog.Info("clearing current failed provision to make way for a new provision")
	return r.clearOutCurrentProvision(cd, cdLog)
}

func (r *ReconcileClusterDeployment) reconcileCompletedProvision(cd *hivev1.ClusterDeployment, provision *hivev1.ClusterProvision, cdLog log.FieldLogger) (reconcile.Result, error) {
	cdLog.Info("provision completed successfully")

	statusChange := false
	if cd.Status.InstalledTimestamp == nil {
		statusChange = true
		now := metav1.Now()
		cd.Status.InstalledTimestamp = &now
	}
	conds, changed := controllerutils.SetClusterDeploymentConditionWithChangeCheck(
		cd.Status.Conditions,
		hivev1.ProvisionFailedCondition,
		corev1.ConditionFalse,
		"ProvisionSucceeded",
		fmt.Sprintf("Provision %s succeeded.", provision.Name),
		controllerutils.UpdateConditionNever,
	)
	if changed {
		statusChange = true
		cd.Status.Conditions = conds
	}
	if statusChange {
		if err := r.Status().Update(context.TODO(), cd); err != nil {
			cdLog.WithError(err).Log(controllerutils.LogLevel(err), "failed to update cluster deployment status")
			return reconcile.Result{}, err
		}
	}

	if cd.Spec.Installed {
		return reconcile.Result{}, nil
	}

	cd.Spec.Installed = true

	if r.protectedDelete {
		// Set protected delete on for the ClusterDeployment.
		// If the ClusterDeployment already has the ProtectedDelete annotation, do not overwrite it. This allows the
		// user an opportunity to explicitly exclude a ClusterDeployment from delete protection at the time of
		// creation of the ClusterDeployment.
		if _, annotationPresent := cd.Annotations[constants.ProtectedDeleteAnnotation]; !annotationPresent {
			initializeAnnotations(cd)
			cd.Annotations[constants.ProtectedDeleteAnnotation] = "true"
		}
	}

	if err := r.Update(context.TODO(), cd); err != nil {
		cdLog.WithError(err).Log(controllerutils.LogLevel(err), "failed to set the Installed flag")
		return reconcile.Result{}, err
	}

	// jobDuration calculates the time elapsed since the first clusterprovision was created
	startTime := cd.CreationTimestamp
	if firstProvision := r.getFirstProvision(cd, cdLog); firstProvision != nil {
		startTime = firstProvision.CreationTimestamp
	}
	jobDuration := time.Since(startTime.Time)
	cdLog.WithField("duration", jobDuration.Seconds()).Debug("install job completed")
	metricInstallJobDuration.Observe(float64(jobDuration.Seconds()))

	// Report a metric for the total number of install restarts:
	metricCompletedInstallJobRestarts.WithLabelValues(hivemetrics.GetClusterDeploymentType(cd)).
		Observe(float64(cd.Status.InstallRestarts))

	metricClustersInstalled.WithLabelValues(hivemetrics.GetClusterDeploymentType(cd)).Inc()

	return reconcile.Result{}, nil
}

func (r *ReconcileClusterDeployment) clearOutCurrentProvision(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) (reconcile.Result, error) {
	cd.Status.ProvisionRef = nil
	cd.Status.InstallRestarts = cd.Status.InstallRestarts + 1
	if err := r.Status().Update(context.TODO(), cd); err != nil {
		cdLog.WithError(err).Log(controllerutils.LogLevel(err), "could not clear out current provision")
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

// getReleaseImage looks for a a release image in clusterdeployment or its corresponding imageset in the following order:
// 1 - specified in the cluster deployment spec.images.releaseImage
// 2 - referenced in the cluster deployment spec.imageSet
func (r *ReconcileClusterDeployment) getReleaseImage(cd *hivev1.ClusterDeployment, imageSet *hivev1.ClusterImageSet, cdLog log.FieldLogger) string {
	if cd.Spec.Provisioning.ReleaseImage != "" {
		return cd.Spec.Provisioning.ReleaseImage
	}
	if imageSet != nil {
		return imageSet.Spec.ReleaseImage
	}
	return ""
}

func (r *ReconcileClusterDeployment) getClusterImageSet(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) (*hivev1.ClusterImageSet, error) {
	if cd.Spec.Provisioning.ImageSetRef == nil || len(cd.Spec.Provisioning.ImageSetRef.Name) == 0 {
		return nil, nil
	}
	imageSet := &hivev1.ClusterImageSet{}
	if err := r.Get(context.TODO(), types.NamespacedName{Name: cd.Spec.Provisioning.ImageSetRef.Name}, imageSet); err != nil {
		if apierrors.IsNotFound(err) {
			cdLog.WithField("clusterimageset", cd.Spec.Provisioning.ImageSetRef.Name).Warning("clusterdeployment references non-existent clusterimageset")
			if err := r.setImageSetNotFoundCondition(cd, true, cdLog); err != nil {
				return nil, err
			}
		} else {
			cdLog.WithError(err).WithField("clusterimageset", cd.Spec.Provisioning.ImageSetRef.Name).Error("unexpected error retrieving clusterimageset")
		}
		return nil, err
	}
	if err := r.setImageSetNotFoundCondition(cd, false, cdLog); err != nil {
		return nil, err
	}
	return imageSet, nil
}

func (r *ReconcileClusterDeployment) statusUpdate(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) error {
	err := r.Status().Update(context.TODO(), cd)
	if err != nil {
		cdLog.WithError(err).Log(controllerutils.LogLevel(err), "cannot update clusterdeployment status")
	}
	return err
}

const (
	imagesResolvedReason = "ImagesResolved"
	imagesResolvedMsg    = "Images required for cluster deployment installations are resolved"
)

func (r *ReconcileClusterDeployment) resolveInstallerImage(cd *hivev1.ClusterDeployment, imageSet *hivev1.ClusterImageSet, releaseImage string, cdLog log.FieldLogger) (*reconcile.Result, error) {
	areImagesResolved := cd.Status.InstallerImage != nil && cd.Status.CLIImage != nil

	jobKey := client.ObjectKey{Namespace: cd.Namespace, Name: imageset.GetImageSetJobName(cd.Name)}
	jobLog := cdLog.WithField("job", jobKey.Name)

	existingJob := &batchv1.Job{}
	switch err := r.Get(context.Background(), jobKey, existingJob); {
	// The job does not exist. If the images have been resolved, continue reconciling. Otherwise, create the job.
	case apierrors.IsNotFound(err):
		if areImagesResolved {
			return nil, r.setInstallImagesNotResolvedCondition(cd, corev1.ConditionFalse, imagesResolvedReason, imagesResolvedMsg, cdLog)
		}

		job := imageset.GenerateImageSetJob(cd, releaseImage, controllerutils.InstallServiceAccountName,
			os.Getenv("HTTP_PROXY"),
			os.Getenv("HTTPS_PROXY"),
			os.Getenv("NO_PROXY"))

		cdLog.WithField("derivedObject", job.Name).Debug("Setting labels on derived object")
		job.Labels = k8slabels.AddLabel(job.Labels, constants.ClusterDeploymentNameLabel, cd.Name)
		job.Labels = k8slabels.AddLabel(job.Labels, constants.JobTypeLabel, constants.JobTypeImageSet)
		if err := controllerutil.SetControllerReference(cd, job, r.scheme); err != nil {
			cdLog.WithError(err).Error("error setting controller reference on job")
			return nil, err
		}

		jobLog.WithField("releaseImage", releaseImage).Info("creating imageset job")
		err = controllerutils.SetupClusterInstallServiceAccount(r, cd.Namespace, cdLog)
		if err != nil {
			cdLog.WithError(err).Log(controllerutils.LogLevel(err), "error setting up service account and role")
			return nil, err
		}

		if err := r.Create(context.TODO(), job); err != nil {
			jobLog.WithError(err).Log(controllerutils.LogLevel(err), "error creating job")
			return nil, err
		}
		// kickstartDuration calculates the delay between creation of cd and start of imageset job
		kickstartDuration := time.Since(cd.CreationTimestamp.Time)
		cdLog.WithField("elapsed", kickstartDuration.Seconds()).Info("calculated time to imageset job seconds")
		metricImageSetDelaySeconds.Observe(float64(kickstartDuration.Seconds()))
		return &reconcile.Result{}, nil

	// There was an error getting the job. Return the error.
	case err != nil:
		jobLog.WithError(err).Error("cannot get job")
		return nil, err

	// The job exists and is in the process of getting deleted. If the images were resolved, then continue reconciling.
	// If the images were not resolved, requeue and wait for the delete to complete.
	case !existingJob.DeletionTimestamp.IsZero():
		if areImagesResolved {
			return nil, r.setInstallImagesNotResolvedCondition(cd, corev1.ConditionFalse, imagesResolvedReason, imagesResolvedMsg, cdLog)
		}
		jobLog.Debug("imageset job is being deleted. Will recreate once deleted")
		return &reconcile.Result{RequeueAfter: defaultRequeueTime}, err

	// If job exists and is finished, delete it. If the images were not resolved, then the job will be re-created.
	case controllerutils.IsFinished(existingJob):
		jobLog.WithField("successful", controllerutils.IsSuccessful(existingJob)).
			Warning("Finished job found. Deleting.")
		if err := r.Delete(
			context.Background(),
			existingJob,
			client.PropagationPolicy(metav1.DeletePropagationForeground),
		); err != nil {
			jobLog.WithError(err).Log(controllerutils.LogLevel(err), "cannot delete imageset job")
			return nil, err
		}
		if areImagesResolved {
			return nil, r.setInstallImagesNotResolvedCondition(cd, corev1.ConditionFalse, imagesResolvedReason, imagesResolvedMsg, cdLog)
		}

		// the job has failed to update the images and therefore
		// we need to update the InstallImagesResolvedCondition to reflect why it failed.
		for _, jcond := range existingJob.Status.Conditions {
			if jcond.Type != batchv1.JobFailed {
				continue
			}

			msg := fmt.Sprintf("The job %s/%s to resolve the image failed because of (%s) %s",
				existingJob.Namespace, existingJob.Name,
				jcond.Reason, jcond.Message,
			)
			return &reconcile.Result{}, r.setInstallImagesNotResolvedCondition(cd, corev1.ConditionTrue, "JobToResolveImagesFailed", msg, cdLog)
		}

		return &reconcile.Result{}, nil

	// The job exists and is in progress. Wait for the job to finish before doing any more reconciliation.
	default:
		jobLog.Debug("job exists and is in progress")
		return &reconcile.Result{}, nil
	}
}

func (r *ReconcileClusterDeployment) setInstallImagesNotResolvedCondition(cd *hivev1.ClusterDeployment, status corev1.ConditionStatus, reason string, message string, cdLog log.FieldLogger) error {
	conditions, changed := controllerutils.SetClusterDeploymentConditionWithChangeCheck(
		cd.Status.Conditions,
		hivev1.InstallImagesNotResolvedCondition,
		status,
		reason,
		message,
		controllerutils.UpdateConditionIfReasonOrMessageChange)
	if !changed {
		return nil
	}
	cd.Status.Conditions = conditions
	cdLog.Debugf("setting InstallImagesNotResolvedCondition to %v", status)
	return r.Status().Update(context.TODO(), cd)
}

func (r *ReconcileClusterDeployment) setDNSNotReadyCondition(cd *hivev1.ClusterDeployment, status corev1.ConditionStatus, reason string, message string, cdLog log.FieldLogger) error {
	conditions, changed := controllerutils.SetClusterDeploymentConditionWithChangeCheck(
		cd.Status.Conditions,
		hivev1.DNSNotReadyCondition,
		status,
		reason,
		message,
		controllerutils.UpdateConditionIfReasonOrMessageChange)
	if !changed {
		return nil
	}
	cd.Status.Conditions = conditions
	cdLog.Debugf("setting DNSNotReadyCondition to %v", status)
	return r.Status().Update(context.TODO(), cd)
}

func (r *ReconcileClusterDeployment) setAuthenticationFailure(cd *hivev1.ClusterDeployment, authSuccessful bool, cdLog log.FieldLogger) (bool, error) {

	var status corev1.ConditionStatus
	var reason, message string

	if authSuccessful {
		status = corev1.ConditionFalse
		reason = platformAuthSuccessReason
		message = "Platform credentials passed authentication check"
	} else {
		status = corev1.ConditionTrue
		reason = platformAuthFailureReason
		message = "Platform credentials failed authentication check"
	}

	conditions, changed := controllerutils.SetClusterDeploymentConditionWithChangeCheck(
		cd.Status.Conditions,
		hivev1.AuthenticationFailureClusterDeploymentCondition,
		status,
		reason,
		message,
		controllerutils.UpdateConditionIfReasonOrMessageChange)
	if !changed {
		return false, nil
	}
	cd.Status.Conditions = conditions

	return changed, r.Status().Update(context.TODO(), cd)
}

func (r *ReconcileClusterDeployment) setInstallLaunchErrorCondition(cd *hivev1.ClusterDeployment, status corev1.ConditionStatus, reason string, message string, cdLog log.FieldLogger) error {
	conditions, changed := controllerutils.SetClusterDeploymentConditionWithChangeCheck(
		cd.Status.Conditions,
		hivev1.InstallLaunchErrorCondition,
		status,
		reason,
		message,
		controllerutils.UpdateConditionIfReasonOrMessageChange)
	if !changed {
		return nil
	}
	cd.Status.Conditions = conditions
	cdLog.WithField("status", status).Debug("setting InstallLaunchErrorCondition")
	return r.Status().Update(context.TODO(), cd)
}

func (r *ReconcileClusterDeployment) setDeprovisionLaunchErrorCondition(cd *hivev1.ClusterDeployment, status corev1.ConditionStatus, reason string, message string, cdLog log.FieldLogger) error {
	conditions, changed := controllerutils.SetClusterDeploymentConditionWithChangeCheck(
		cd.Status.Conditions,
		hivev1.DeprovisionLaunchErrorCondition,
		status,
		reason,
		message,
		controllerutils.UpdateConditionIfReasonOrMessageChange)
	if !changed {
		return nil
	}
	cd.Status.Conditions = conditions
	cdLog.WithField("status", status).Debug("setting DeprovisionLaunchErrorCondition")
	return r.Status().Update(context.TODO(), cd)
}

func (r *ReconcileClusterDeployment) setImageSetNotFoundCondition(cd *hivev1.ClusterDeployment, isNotFound bool, cdLog log.FieldLogger) error {
	status := corev1.ConditionFalse
	reason := clusterImageSetFoundReason
	message := fmt.Sprintf("ClusterImageSet %s is available", cd.Spec.Provisioning.ImageSetRef.Name)
	if isNotFound {
		status = corev1.ConditionTrue
		reason = clusterImageSetNotFoundReason
		message = fmt.Sprintf("ClusterImageSet %s is not available", cd.Spec.Provisioning.ImageSetRef.Name)
	}
	conds, changed := controllerutils.SetClusterDeploymentConditionWithChangeCheck(
		cd.Status.Conditions,
		hivev1.ClusterImageSetNotFoundCondition,
		status,
		reason,
		message,
		controllerutils.UpdateConditionNever)
	if !changed {
		return nil
	}
	cdLog.Infof("setting ClusterImageSetNotFoundCondition to %v", status)
	cd.Status.Conditions = conds
	err := r.Status().Update(context.TODO(), cd)
	if err != nil {
		cdLog.WithError(err).Log(controllerutils.LogLevel(err), "cannot update status conditions")
	}
	return err
}

// setClusterStatusURLs fetches the openshift console route from the remote cluster and uses it to determine
// the correct APIURL and WebConsoleURL, and then set them in the Status. Typically only called if these Status fields
// are unset.
func (r *ReconcileClusterDeployment) setClusterStatusURLs(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) (reconcile.Result, error) {
	server, err := remoteclient.InitialURL(r.Client, cd)
	if err != nil {
		cdLog.WithError(err).Error("could not get API URL from kubeconfig")
		return reconcile.Result{}, err
	}
	cdLog.Debugf("found cluster API URL in kubeconfig: %s", server)
	cd.Status.APIURL = server

	remoteClient, unreachable, requeue := remoteclient.ConnectToRemoteCluster(
		cd,
		r.remoteClusterAPIClientBuilder(cd),
		r.Client,
		cdLog,
	)
	if unreachable {
		return reconcile.Result{Requeue: requeue}, nil
	}

	routeObject := &routev1.Route{}
	if err := remoteClient.Get(
		context.Background(),
		client.ObjectKey{Namespace: "openshift-console", Name: "console"},
		routeObject,
	); err != nil {
		cdLog.WithError(err).Info("error fetching remote route object")
		return reconcile.Result{Requeue: true}, nil
	}
	cdLog.Debugf("read remote route object: %s", routeObject)
	cd.Status.WebConsoleURL = "https://" + routeObject.Spec.Host

	if err := r.Status().Update(context.TODO(), cd); err != nil {
		cdLog.WithError(err).Log(controllerutils.LogLevel(err), "could not set cluster status URLs")
		return reconcile.Result{Requeue: true}, nil
	}

	return reconcile.Result{}, nil
}

// ensureManagedDNSZoneDeleted is a safety check to ensure that the child managed DNSZone
// linked to the parent cluster deployment gets a deletionTimestamp when the parent is deleted.
// Normally we expect Kube garbage collection to do this for us, but in rare cases we've seen it
// not working as intended.
func (r *ReconcileClusterDeployment) ensureManagedDNSZoneDeleted(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) (gone bool, returnErr error) {
	if !cd.Spec.ManageDNS {
		return true, nil
	}
	dnsZone := &hivev1.DNSZone{}
	dnsZoneNamespacedName := types.NamespacedName{Namespace: cd.Namespace, Name: controllerutils.DNSZoneName(cd.Name)}
	switch err := r.Get(context.TODO(), dnsZoneNamespacedName, dnsZone); {
	case apierrors.IsNotFound(err):
		cdLog.Debug("dnszone has been removed from storage")
		return true, nil
	case err != nil:
		cdLog.WithError(err).Error("error looking up managed dnszone")
		return false, err
	case !dnsZone.DeletionTimestamp.IsZero():
		cdLog.Debug("dnszone has been deleted but is still in storage")
		return false, nil
	}
	if err := r.Delete(context.TODO(), dnsZone); err != nil {
		cdLog.WithError(err).Log(controllerutils.LogLevel(err), "error deleting managed dnszone")
		return false, err
	}
	return false, nil
}

func (r *ReconcileClusterDeployment) ensureClusterDeprovisioned(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) (deprovisioned bool, returnErr error) {
	// Skips creation of deprovision request if PreserveOnDelete is true and cluster is installed
	if cd.Spec.PreserveOnDelete {
		if cd.Spec.Installed {
			cdLog.Warn("skipping creation of deprovisioning request for installed cluster due to PreserveOnDelete=true")
			return true, nil
		}
		// Overriding PreserveOnDelete because we might have deleted the cluster deployment before it finished
		// installing, which can cause AWS resources to leak
		cdLog.Info("PreserveOnDelete=true but creating deprovisioning request as cluster was never successfully provisioned")
	}

	if cd.Spec.ClusterMetadata == nil {
		cdLog.Warn("skipping uninstall for cluster that never had clusterID set")
		return true, nil
	}

	// We do not yet support deprovision for BareMetal, for now skip deprovision and remove finalizer.
	if cd.Spec.Platform.BareMetal != nil {
		cdLog.Info("skipping deprovision for BareMetal cluster, removing finalizer")
		return true, nil
	}

	// Generate a deprovision request
	request, err := generateDeprovision(cd)
	if err != nil {
		cdLog.WithError(err).Error("error generating deprovision request")
		return false, err
	}

	cdLog.WithField("derivedObject", request.Name).Debug("Setting label on derived object")
	request.Labels = k8slabels.AddLabel(request.Labels, constants.ClusterDeploymentNameLabel, cd.Name)
	err = controllerutil.SetControllerReference(cd, request, r.scheme)
	if err != nil {
		cdLog.Errorf("error setting controller reference on deprovision request: %v", err)
		return false, err
	}

	// Check if deprovision request already exists:
	existingRequest := &hivev1.ClusterDeprovision{}
	switch err = r.Get(context.TODO(), types.NamespacedName{Name: cd.Name, Namespace: cd.Namespace}, existingRequest); {
	case apierrors.IsNotFound(err):
		cdLog.Info("creating deprovision request for cluster deployment")
		switch err = r.Create(context.TODO(), request); {
		case apierrors.IsAlreadyExists(err):
			cdLog.Info("deprovision request already exists")
			return false, nil
		case err != nil:
			cdLog.WithError(err).Log(controllerutils.LogLevel(err), "error creating deprovision request")
			// Check if namespace is terminated, if so we can give up, remove the finalizer, and let
			// the cluster go away.
			ns := &corev1.Namespace{}
			err = r.Get(context.TODO(), types.NamespacedName{Name: cd.Namespace}, ns)
			if err != nil {
				cdLog.WithError(err).Error("error checking for deletionTimestamp on namespace")
				return false, err
			}
			if ns.DeletionTimestamp != nil {
				cdLog.Warn("detected a namespace deleted before deprovision request could be created, giving up on deprovision and removing finalizer")
				return true, err
			}
			return false, err
		default:
			return false, nil
		}
	case err != nil:
		cdLog.WithError(err).Error("error getting deprovision request")
		return false, err
	}

	authenticationFailureCondition := controllerutils.FindClusterDeprovisionCondition(existingRequest.Status.Conditions, hivev1.AuthenticationFailureClusterDeprovisionCondition)
	if authenticationFailureCondition != nil {
		err := r.setDeprovisionLaunchErrorCondition(cd,
			authenticationFailureCondition.Status,
			authenticationFailureCondition.Reason,
			authenticationFailureCondition.Message,
			cdLog)

		if err != nil {
			cdLog.WithError(err).Log(controllerutils.LogLevel(err), "could not update deprovisionLaunchErrorCondition")
			return false, err
		}
	}

	if !existingRequest.Status.Completed {
		cdLog.Debug("deprovision request not yet completed")
		return false, nil
	}

	return true, nil
}

func (r *ReconcileClusterDeployment) syncDeletedClusterDeployment(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) (reconcile.Result, error) {
	switch _, relocateStatus, err := controllerutils.IsRelocating(cd); {
	case err != nil:
		cdLog.WithError(err).Error("could not determine relocate status")
		return reconcile.Result{}, errors.Wrap(err, "could not determine relocate status")
	case relocateStatus == hivev1.RelocateComplete:
		cdLog.Info("clusterdeployment relocated, removing finalizer")
		err := r.removeClusterDeploymentFinalizer(cd, cdLog)
		if err != nil {
			cdLog.WithError(err).Log(controllerutils.LogLevel(err), "error removing finalizer")
		}
		return reconcile.Result{}, err
	case relocateStatus != "":
		cdLog.Debug("ClusterDeployment is in the middle of a relocate. Wait until relocate has been completed or aborted before doing finalization.")
		return reconcile.Result{}, nil
	}

	if controllerutils.IsDeleteProtected(cd) {
		cdLog.Error("deprovision blocked for ClusterDeployment with protected delete on")
		return reconcile.Result{}, nil
	}

	dnsZoneGone, err := r.ensureManagedDNSZoneDeleted(cd, cdLog)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Wait for outstanding provision to be removed before creating deprovision request
	switch result, err := r.stopProvisioning(cd, cdLog); {
	case result != nil:
		return *result, err
	case err != nil:
		return reconcile.Result{}, err
	}

	deprovisioned, err := r.ensureClusterDeprovisioned(cd, cdLog)
	if err != nil {
		return reconcile.Result{}, err
	}

	switch {
	case !deprovisioned:
		return reconcile.Result{}, nil
	case !dnsZoneGone:
		return reconcile.Result{RequeueAfter: defaultRequeueTime}, nil
	default:
		cdLog.Infof("DNSZone gone and deprovision request completed, removing finalizer")
		if err := r.removeClusterDeploymentFinalizer(cd, cdLog); err != nil {
			cdLog.WithError(err).Log(controllerutils.LogLevel(err), "error removing finalizer")
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}
}

func (r *ReconcileClusterDeployment) stopProvisioning(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) (*reconcile.Result, error) {
	if cd.Status.ProvisionRef == nil {
		return nil, nil
	}
	provision := &hivev1.ClusterProvision{}
	switch err := r.Get(context.TODO(), types.NamespacedName{Name: cd.Status.ProvisionRef.Name, Namespace: cd.Namespace}, provision); {
	case apierrors.IsNotFound(err):
		cdLog.Debug("linked provision removed")
		return nil, nil
	case err != nil:
		cdLog.WithError(err).Error("could not get provision")
		return nil, err
	case provision.DeletionTimestamp == nil:
		if err := r.Delete(context.TODO(), provision); err != nil {
			cdLog.WithError(err).Log(controllerutils.LogLevel(err), "could not delete provision")
			return nil, err
		}
		cdLog.Info("deleted outstanding provision")
		return &reconcile.Result{RequeueAfter: defaultRequeueTime}, nil
	default:
		cdLog.Debug("still waiting for outstanding provision to be removed")
		return &reconcile.Result{RequeueAfter: defaultRequeueTime}, nil
	}
}

func (r *ReconcileClusterDeployment) addClusterDeploymentFinalizer(cd *hivev1.ClusterDeployment) error {
	cd = cd.DeepCopy()
	controllerutils.AddFinalizer(cd, hivev1.FinalizerDeprovision)
	return r.Update(context.TODO(), cd)
}

func (r *ReconcileClusterDeployment) removeClusterDeploymentFinalizer(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) error {

	cd = cd.DeepCopy()
	controllerutils.DeleteFinalizer(cd, hivev1.FinalizerDeprovision)
	if err := r.Update(context.TODO(), cd); err != nil {
		return err
	}

	clearDeprovisionUnderwaySecondsMetric(cd, cdLog)

	// Increment the clusters deleted counter:
	metricClustersDeleted.WithLabelValues(hivemetrics.GetClusterDeploymentType(cd)).Inc()

	return nil
}

// setDNSDelayMetric will calculate the amount of time elapsed from clusterdeployment creation
// to when the dnszone became ready, and set a metric to report the delay.
// Will return a bool indicating whether the clusterdeployment has been modified, and whether any error was encountered.
func (r *ReconcileClusterDeployment) setDNSDelayMetric(cd *hivev1.ClusterDeployment, dnsZone *hivev1.DNSZone, cdLog log.FieldLogger) (bool, error) {
	modified := false
	initializeAnnotations(cd)

	if _, ok := cd.Annotations[dnsReadyAnnotation]; ok {
		// already have recorded the dnsdelay metric
		return modified, nil
	}

	readyTimestamp := dnsReadyTransitionTime(dnsZone)
	if readyTimestamp == nil {
		msg := "did not find timestamp for when dnszone became ready"
		cdLog.WithField("dnszone", dnsZone.Name).Error(msg)
		return modified, fmt.Errorf(msg)
	}

	dnsDelayDuration := readyTimestamp.Sub(cd.CreationTimestamp.Time)
	cdLog.WithField("duration", dnsDelayDuration.Seconds()).Info("DNS ready")
	cd.Annotations[dnsReadyAnnotation] = dnsDelayDuration.String()
	if err := r.Update(context.TODO(), cd); err != nil {
		cdLog.WithError(err).Log(controllerutils.LogLevel(err), "failed to save annotation marking DNS becoming ready")
		return modified, err
	}
	modified = true

	metricDNSDelaySeconds.Observe(float64(dnsDelayDuration.Seconds()))

	return modified, nil
}

func (r *ReconcileClusterDeployment) ensureManagedDNSZone(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) (*hivev1.DNSZone, error) {
	switch p := cd.Spec.Platform; {
	case p.AWS != nil:
	case p.GCP != nil:
	case p.Azure != nil:
	default:
		cdLog.Error("cluster deployment platform does not support managed DNS")
		if err := r.setDNSNotReadyCondition(cd, corev1.ConditionTrue, dnsUnsupportedPlatformReason, "Managed DNS is not supported on specified platform", cdLog); err != nil {
			cdLog.WithError(err).Log(controllerutils.LogLevel(err), "could not update DNSNotReadyCondition for DNSUnsupportedPlatform reason")
			return nil, err
		}
		return nil, errors.New("managed DNS not supported on platform")
	}

	dnsZone := &hivev1.DNSZone{}
	dnsZoneNamespacedName := types.NamespacedName{Namespace: cd.Namespace, Name: controllerutils.DNSZoneName(cd.Name)}
	logger := cdLog.WithField("zone", dnsZoneNamespacedName.String())

	switch err := r.Get(context.TODO(), dnsZoneNamespacedName, dnsZone); {
	case apierrors.IsNotFound(err):
		logger.Info("creating new DNSZone for cluster deployment")
		return nil, r.createManagedDNSZone(cd, logger)
	case err != nil:
		logger.WithError(err).Error("failed to fetch DNS zone")
		return nil, err
	}

	if !metav1.IsControlledBy(dnsZone, cd) {
		cdLog.Error("DNS zone already exists but is not owned by cluster deployment")
		if err := r.setDNSNotReadyCondition(cd, corev1.ConditionTrue, dnsZoneResourceConflictReason, "Existing DNS zone not owned by cluster deployment", cdLog); err != nil {
			cdLog.WithError(err).Log(controllerutils.LogLevel(err), "could not update DNSNotReadyCondition")
			return nil, err
		}
		return nil, errors.New("Existing unowned DNS zone")
	}

	availableCondition := controllerutils.FindDNSZoneCondition(dnsZone.Status.Conditions, hivev1.ZoneAvailableDNSZoneCondition)
	insufficientCredentialsCondition := controllerutils.FindDNSZoneCondition(dnsZone.Status.Conditions, hivev1.InsufficientCredentialsCondition)
	authenticationFailureCondition := controllerutils.FindDNSZoneCondition(dnsZone.Status.Conditions, hivev1.AuthenticationFailureCondition)
	var (
		status          corev1.ConditionStatus
		reason, message string
	)
	switch {
	case availableCondition != nil && availableCondition.Status == corev1.ConditionTrue:
		status = corev1.ConditionFalse
		reason = dnsReadyReason
		message = "DNS Zone available"
	case insufficientCredentialsCondition != nil && insufficientCredentialsCondition.Status == corev1.ConditionTrue:
		status = corev1.ConditionTrue
		reason = "InsufficientCredentials"
		message = insufficientCredentialsCondition.Message
	case authenticationFailureCondition != nil && authenticationFailureCondition.Status == corev1.ConditionTrue:
		status = corev1.ConditionTrue
		reason = "AuthenticationFailure"
		message = authenticationFailureCondition.Message
	default:
		status = corev1.ConditionTrue
		reason = dnsNotReadyReason
		message = "DNS Zone not yet available"

		isDNSNotReadyConditionSet, dnsNotReadyCondition := isDNSNotReadyConditionSet(cd)
		if isDNSNotReadyConditionSet {
			// Timeout if it has been in this state for longer than allowed.
			timeSinceLastTransition := time.Since(dnsNotReadyCondition.LastTransitionTime.Time)
			if timeSinceLastTransition >= defaultDNSNotReadyTimeout {
				// We've timed out, set the dnsNotReadyTimedoutReason for the DNSNotReady condition
				cdLog.WithField("timeout", defaultDNSNotReadyTimeout).Warn("Timed out waiting on managed dns creation")
				reason = dnsNotReadyTimedoutReason
				message = "DNS Zone timed out in DNSNotReady state"
			}
		}
	}
	if err := r.setDNSNotReadyCondition(cd, status, reason, message, cdLog); err != nil {
		cdLog.WithError(err).Log(controllerutils.LogLevel(err), "could not update DNSNotReadyCondition")
		return nil, err
	}

	if reason != dnsReadyReason {
		return nil, nil
	}
	return dnsZone, nil
}

func (r *ReconcileClusterDeployment) createManagedDNSZone(cd *hivev1.ClusterDeployment, logger log.FieldLogger) error {
	dnsZone := &hivev1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controllerutils.DNSZoneName(cd.Name),
			Namespace: cd.Namespace,
		},
		Spec: hivev1.DNSZoneSpec{
			Zone:               cd.Spec.BaseDomain,
			LinkToParentDomain: true,
		},
	}

	switch {
	case cd.Spec.Platform.AWS != nil:
		additionalTags := make([]hivev1.AWSResourceTag, 0, len(cd.Spec.Platform.AWS.UserTags))
		for k, v := range cd.Spec.Platform.AWS.UserTags {
			additionalTags = append(additionalTags, hivev1.AWSResourceTag{Key: k, Value: v})
		}
		region := ""
		if strings.HasPrefix(cd.Spec.Platform.AWS.Region, constants.AWSChinaRegionPrefix) {
			region = constants.AWSChinaRoute53Region
		}
		dnsZone.Spec.AWS = &hivev1.AWSDNSZoneSpec{
			CredentialsSecretRef:  cd.Spec.Platform.AWS.CredentialsSecretRef,
			CredentialsAssumeRole: cd.Spec.Platform.AWS.CredentialsAssumeRole,
			AdditionalTags:        additionalTags,
			Region:                region,
		}
	case cd.Spec.Platform.GCP != nil:
		dnsZone.Spec.GCP = &hivev1.GCPDNSZoneSpec{
			CredentialsSecretRef: cd.Spec.Platform.GCP.CredentialsSecretRef,
		}
	case cd.Spec.Platform.Azure != nil:
		dnsZone.Spec.Azure = &hivev1.AzureDNSZoneSpec{
			CredentialsSecretRef: cd.Spec.Platform.Azure.CredentialsSecretRef,
			ResourceGroupName:    cd.Spec.Platform.Azure.BaseDomainResourceGroupName,
		}
	}

	logger.WithField("derivedObject", dnsZone.Name).Debug("Setting labels on derived object")
	dnsZone.Labels = k8slabels.AddLabel(dnsZone.Labels, constants.ClusterDeploymentNameLabel, cd.Name)
	dnsZone.Labels = k8slabels.AddLabel(dnsZone.Labels, constants.DNSZoneTypeLabel, constants.DNSZoneTypeChild)
	if err := controllerutil.SetControllerReference(cd, dnsZone, r.scheme); err != nil {
		logger.WithError(err).Error("error setting controller reference on dnszone")
		return err
	}

	err := r.Create(context.TODO(), dnsZone)
	if err != nil {
		logger.WithError(err).Log(controllerutils.LogLevel(err), "cannot create DNS zone")
		return err
	}
	logger.Info("dns zone created")
	return nil
}

func selectorPodWatchHandler(a client.Object) []reconcile.Request {
	retval := []reconcile.Request{}

	pod := a.(*corev1.Pod)
	if pod == nil {
		// Wasn't a Pod, bail out. This should not happen.
		log.Errorf("Error converting MapObject.Object to Pod. Value: %+v", a)
		return retval
	}
	if pod.Labels == nil {
		return retval
	}
	cdName, ok := pod.Labels[constants.ClusterDeploymentNameLabel]
	if !ok {
		return retval
	}
	retval = append(retval, reconcile.Request{NamespacedName: types.NamespacedName{
		Name:      cdName,
		Namespace: pod.Namespace,
	}})
	return retval
}

// GetInstallLogsPVCName returns the expected name of the persistent volume claim for cluster install failure logs.
// TODO: Remove this function and all calls to it. It's being left here for compatibility until the install log PVs are removed from all the installs.
func GetInstallLogsPVCName(cd *hivev1.ClusterDeployment) string {
	return apihelpers.GetResourceName(cd.Name, "install-logs")
}

// cleanupInstallLogPVC will immediately delete the PVC (should it exist) if the cluster was installed successfully, without retries.
// If there were retries, it will delete the PVC if it has been more than 7 days since the job was completed.
// TODO: Remove this function and all calls to it. It's being left here for compatibility until the install log PVs are removed from all the installs.
func (r *ReconcileClusterDeployment) cleanupInstallLogPVC(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) error {
	if !cd.Spec.Installed {
		return nil
	}

	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(context.TODO(), types.NamespacedName{Name: GetInstallLogsPVCName(cd), Namespace: cd.Namespace}, pvc)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		cdLog.WithError(err).Error("error looking up install logs PVC")
		return err
	}

	// Also check if we've already deleted it, pvc won't be deleted until the install pod is, and that is retained
	// for one day.
	if pvc.DeletionTimestamp != nil {
		return nil
	}

	pvcLog := cdLog.WithField("pvc", pvc.Name)

	switch {
	case cd.Status.InstallRestarts == 0:
		pvcLog.Info("deleting logs PersistentVolumeClaim for installed cluster with no restarts")
	case cd.Status.InstalledTimestamp == nil:
		pvcLog.Warn("deleting logs PersistentVolumeClaim for cluster with errors but no installed timestamp")
	// Otherwise, delete if more than 7 days have passed.
	case time.Since(cd.Status.InstalledTimestamp.Time) > (7 * 24 * time.Hour):
		pvcLog.Info("deleting logs PersistentVolumeClaim for cluster that was installed after restarts more than 7 days ago")
	default:
		cdLog.WithField("pvc", pvc.Name).Debug("preserving logs PersistentVolumeClaim for cluster with install restarts for 7 days")
		return nil
	}

	if err := r.Delete(context.TODO(), pvc); err != nil {
		pvcLog.WithError(err).Log(controllerutils.LogLevel(err), "error deleting install logs PVC")
		return err
	}
	return nil
}

func generateDeprovision(cd *hivev1.ClusterDeployment) (*hivev1.ClusterDeprovision, error) {
	req := &hivev1.ClusterDeprovision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cd.Name,
			Namespace: cd.Namespace,
		},
		Spec: hivev1.ClusterDeprovisionSpec{
			InfraID:   cd.Spec.ClusterMetadata.InfraID,
			ClusterID: cd.Spec.ClusterMetadata.ClusterID,
		},
	}

	switch {
	case cd.Spec.Platform.AWS != nil:
		req.Spec.Platform.AWS = &hivev1.AWSClusterDeprovision{
			Region:                cd.Spec.Platform.AWS.Region,
			CredentialsSecretRef:  &cd.Spec.Platform.AWS.CredentialsSecretRef,
			CredentialsAssumeRole: cd.Spec.Platform.AWS.CredentialsAssumeRole,
		}
	case cd.Spec.Platform.Azure != nil:
		req.Spec.Platform.Azure = &hivev1.AzureClusterDeprovision{
			CredentialsSecretRef: &cd.Spec.Platform.Azure.CredentialsSecretRef,
		}
	case cd.Spec.Platform.GCP != nil:
		req.Spec.Platform.GCP = &hivev1.GCPClusterDeprovision{
			Region:               cd.Spec.Platform.GCP.Region,
			CredentialsSecretRef: &cd.Spec.Platform.GCP.CredentialsSecretRef,
		}
	case cd.Spec.Platform.OpenStack != nil:
		req.Spec.Platform.OpenStack = &hivev1.OpenStackClusterDeprovision{
			Cloud:                 cd.Spec.Platform.OpenStack.Cloud,
			CredentialsSecretRef:  &cd.Spec.Platform.OpenStack.CredentialsSecretRef,
			CertificatesSecretRef: cd.Spec.Platform.OpenStack.CertificatesSecretRef,
		}
	case cd.Spec.Platform.VSphere != nil:
		req.Spec.Platform.VSphere = &hivev1.VSphereClusterDeprovision{
			CredentialsSecretRef:  cd.Spec.Platform.VSphere.CredentialsSecretRef,
			CertificatesSecretRef: cd.Spec.Platform.VSphere.CertificatesSecretRef,
			VCenter:               cd.Spec.Platform.VSphere.VCenter,
		}
	case cd.Spec.Platform.Ovirt != nil:
		req.Spec.Platform.Ovirt = &hivev1.OvirtClusterDeprovision{
			CredentialsSecretRef:  cd.Spec.Platform.Ovirt.CredentialsSecretRef,
			CertificatesSecretRef: cd.Spec.Platform.Ovirt.CertificatesSecretRef,
			ClusterID:             cd.Spec.Platform.Ovirt.ClusterID,
		}
	default:
		return nil, errors.New("unsupported cloud provider for deprovision")
	}

	return req, nil
}

func generatePullSecretObj(pullSecret string, pullSecretName string, cd *hivev1.ClusterDeployment) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: corev1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      pullSecretName,
			Namespace: cd.Namespace,
		},
		Type: corev1.SecretTypeDockerConfigJson,
		StringData: map[string]string{
			corev1.DockerConfigJsonKey: pullSecret,
		},
	}
}

func dnsReadyTransitionTime(dnsZone *hivev1.DNSZone) *time.Time {
	readyCondition := controllerutils.FindDNSZoneCondition(dnsZone.Status.Conditions, hivev1.ZoneAvailableDNSZoneCondition)

	if readyCondition != nil && readyCondition.Status == corev1.ConditionTrue {
		return &readyCondition.LastTransitionTime.Time
	}

	return nil
}

func clearDeprovisionUnderwaySecondsMetric(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) {
	cleared := hivemetrics.MetricClusterDeploymentDeprovisioningUnderwaySeconds.Delete(map[string]string{
		"cluster_deployment": cd.Name,
		"namespace":          cd.Namespace,
		"cluster_type":       hivemetrics.GetClusterDeploymentType(cd),
	})
	if cleared {
		cdLog.Debug("cleared metric: %v", hivemetrics.MetricClusterDeploymentDeprovisioningUnderwaySeconds)
	}
}

// initializeAnnotations() initializes the annotations if it is not already
func initializeAnnotations(cd *hivev1.ClusterDeployment) {
	if cd.Annotations == nil {
		cd.Annotations = map[string]string{}
	}
}

// mergePullSecrets merges the global pull secret JSON (if defined) with the cluster's pull secret JSON (if defined)
// An error will be returned if neither is defined
func (r *ReconcileClusterDeployment) mergePullSecrets(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) (string, error) {
	var localPullSecret string
	var err error

	// For code readability let's call the pull secret in cluster deployment config as local pull secret
	if cd.Spec.PullSecretRef != nil {
		localPullSecret, err = controllerutils.LoadSecretData(r.Client, cd.Spec.PullSecretRef.Name, cd.Namespace, corev1.DockerConfigJsonKey)
		if err != nil {
			return "", errors.Wrap(err, "local pull secret could not be retrieved")
		}
	}

	// Check if global pull secret from env as it comes from hive config
	globalPullSecretName := os.Getenv(constants.GlobalPullSecret)
	var globalPullSecret string
	if len(globalPullSecretName) != 0 {
		globalPullSecret, err = controllerutils.LoadSecretData(r.Client, globalPullSecretName, controllerutils.GetHiveNamespace(), corev1.DockerConfigJsonKey)
		if err != nil {
			return "", errors.Wrap(err, "global pull secret could not be retrieved")
		}
	}

	switch {
	case globalPullSecret != "" && localPullSecret != "":
		// Merge local pullSecret and globalPullSecret. If both pull secrets have same registry name
		// then the merged pull secret will have registry secret from local pull secret
		pullSecret, err := controllerutils.MergeJsons(globalPullSecret, localPullSecret, cdLog)
		if err != nil {
			errMsg := "unable to merge global pull secret with local pull secret"
			cdLog.WithError(err).Error(errMsg)
			return "", errors.Wrap(err, errMsg)
		}
		return pullSecret, nil
	case globalPullSecret != "":
		return globalPullSecret, nil
	case localPullSecret != "":
		return localPullSecret, nil
	default:
		errMsg := "clusterdeployment must specify pull secret since hiveconfig does not specify a global pull secret"
		cdLog.Error(errMsg)
		return "", errors.New(errMsg)
	}
}

// updatePullSecretInfo creates or updates the merged pull secret for the clusterdeployment.
// It returns true when the merged pull secret has been created or updated.
func (r *ReconcileClusterDeployment) updatePullSecretInfo(pullSecret string, cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) (bool, error) {
	var err error
	pullSecretObjExists := true
	existingPullSecretObj := &corev1.Secret{}
	mergedSecretName := constants.GetMergedPullSecretName(cd)
	err = r.Get(context.TODO(), types.NamespacedName{Name: mergedSecretName, Namespace: cd.Namespace}, existingPullSecretObj)
	if err != nil {
		if apierrors.IsNotFound(err) {
			cdLog.Info("Existing pull secret object not found")
			pullSecretObjExists = false
		} else {
			return false, errors.Wrap(err, "Error getting pull secret from cluster deployment")
		}
	}

	if pullSecretObjExists {
		existingPullSecret, ok := existingPullSecretObj.Data[corev1.DockerConfigJsonKey]
		if !ok {
			return false, fmt.Errorf("Pull secret %s did not contain key %s", mergedSecretName, corev1.DockerConfigJsonKey)
		}
		if string(existingPullSecret) == pullSecret {
			cdLog.Debug("Existing and the new merged pull secret are same")
			return false, nil
		}
		cdLog.Info("Existing merged pull secret hash did not match with latest merged pull secret")
		existingPullSecretObj.Data[corev1.DockerConfigJsonKey] = []byte(pullSecret)
		err = r.Update(context.TODO(), existingPullSecretObj)
		if err != nil {
			return false, errors.Wrap(err, "error updating merged pull secret object")
		}
		cdLog.WithField("secretName", mergedSecretName).Info("Updated the merged pull secret object successfully")
	} else {

		// create a new pull secret object
		newPullSecretObj := generatePullSecretObj(
			pullSecret,
			mergedSecretName,
			cd,
		)

		cdLog.WithField("derivedObject", newPullSecretObj.Name).Debug("Setting labels on derived object")
		newPullSecretObj.Labels = k8slabels.AddLabel(newPullSecretObj.Labels, constants.ClusterDeploymentNameLabel, cd.Name)
		newPullSecretObj.Labels = k8slabels.AddLabel(newPullSecretObj.Labels, constants.SecretTypeLabel, constants.SecretTypeMergedPullSecret)
		err = controllerutil.SetControllerReference(cd, newPullSecretObj, r.scheme)
		if err != nil {
			cdLog.Errorf("error setting controller reference on new merged pull secret: %v", err)
			return false, err
		}
		err = r.Create(context.TODO(), newPullSecretObj)
		if err != nil {
			return false, errors.Wrap(err, "error creating new pull secret object")
		}
		cdLog.WithField("secretName", mergedSecretName).Info("Created the merged pull secret object successfully")
	}
	return true, nil
}

func calculateNextProvisionTime(failureTime time.Time, retries int, cdLog log.FieldLogger) time.Time {
	// (2^currentRetries) * 60 seconds up to a max of 24 hours.
	const sleepCap = 24 * time.Hour
	const retryCap = 11 // log_2_(24*60)

	if retries >= retryCap {
		return failureTime.Add(sleepCap)
	}
	return failureTime.Add((1 << uint(retries)) * time.Minute)
}

func (r *ReconcileClusterDeployment) existingProvisions(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) ([]*hivev1.ClusterProvision, error) {
	provisionList := &hivev1.ClusterProvisionList{}
	if err := r.List(
		context.TODO(),
		provisionList,
		client.InNamespace(cd.Namespace),
		client.MatchingLabels(map[string]string{constants.ClusterDeploymentNameLabel: cd.Name}),
	); err != nil {
		cdLog.WithError(err).Warn("could not list provisions for clusterdeployment")
		return nil, err
	}
	provisions := make([]*hivev1.ClusterProvision, len(provisionList.Items))
	for i := range provisionList.Items {
		provisions[i] = &provisionList.Items[i]
	}
	return provisions, nil
}

func (r *ReconcileClusterDeployment) getFirstProvision(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) *hivev1.ClusterProvision {
	provisions, err := r.existingProvisions(cd, cdLog)
	if err != nil {
		return nil
	}
	for _, provision := range provisions {
		if provision.Spec.Attempt == 0 {
			return provision
		}
	}
	cdLog.Warn("could not find the first provision for clusterdeployment")
	return nil
}

func (r *ReconcileClusterDeployment) adoptProvision(cd *hivev1.ClusterDeployment, provision *hivev1.ClusterProvision, cdLog log.FieldLogger) error {
	pLog := cdLog.WithField("provision", provision.Name)
	cd.Status.ProvisionRef = &corev1.LocalObjectReference{Name: provision.Name}
	if cd.Status.InstallStartedTimestamp == nil {
		n := metav1.Now()
		cd.Status.InstallStartedTimestamp = &n
	}
	if err := r.Status().Update(context.TODO(), cd); err != nil {
		pLog.WithError(err).Log(controllerutils.LogLevel(err), "could not adopt provision")
		return err
	}
	pLog.Info("adopted provision")
	return nil
}

func (r *ReconcileClusterDeployment) deleteStaleProvisions(provs []*hivev1.ClusterProvision, cdLog log.FieldLogger) {
	// Cap the number of existing provisions. Always keep the earliest provision as
	// it is used to determine the total time that it took to install. Take off
	// one extra to make room for the new provision being started.
	amountToDelete := len(provs) - maxProvisions
	if amountToDelete <= 0 {
		return
	}
	cdLog.Infof("Deleting %d old provisions", amountToDelete)
	sort.Slice(provs, func(i, j int) bool { return provs[i].Spec.Attempt < provs[j].Spec.Attempt })
	for _, provision := range provs[1 : amountToDelete+1] {
		pLog := cdLog.WithField("provision", provision.Name)
		pLog.Info("Deleting old provision")
		if err := r.Delete(context.TODO(), provision); err != nil {
			pLog.WithError(err).Log(controllerutils.LogLevel(err), "failed to delete old provision")
		}
	}
}

// deleteOldFailedProvisions deletes the failed provisions which are more than 7 days old
func (r *ReconcileClusterDeployment) deleteOldFailedProvisions(provs []*hivev1.ClusterProvision, cdLog log.FieldLogger) {
	cdLog.Debugf("Deleting failed provisions which are more than 7 days old")
	for _, provision := range provs {
		if provision.Spec.Stage == hivev1.ClusterProvisionStageFailed && time.Since(provision.CreationTimestamp.Time) > (7*24*time.Hour) {
			pLog := cdLog.WithField("provision", provision.Name)
			pLog.Info("Deleting failed provision")
			if err := r.Delete(context.TODO(), provision); err != nil {
				pLog.WithError(err).Log(controllerutils.LogLevel(err), "failed to delete failed provision")
			}
		}
	}
}

// validatePlatformCreds ensure the platform/cloud credentials are at least good enough to authenticate with
func (r *ReconcileClusterDeployment) validatePlatformCreds(cd *hivev1.ClusterDeployment, logger log.FieldLogger) (bool, error) {
	return r.validateCredentialsForClusterDeployment(r.Client, cd, logger)
}

// checkForFailedSync returns true if it finds that the ClusterSync has the Failed condition set
func checkForFailedSync(clusterSync *hiveintv1alpha1.ClusterSync) bool {
	for _, cond := range clusterSync.Status.Conditions {
		if cond.Type == hiveintv1alpha1.ClusterSyncFailed {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// setSyncSetFailedCondition updates the hivev1.SyncSetFailedCondition
func (r *ReconcileClusterDeployment) setSyncSetFailedCondition(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) error {
	var (
		status          corev1.ConditionStatus
		reason, message string
	)
	clusterSync := &hiveintv1alpha1.ClusterSync{}
	switch err := r.Get(context.Background(), types.NamespacedName{Namespace: cd.Namespace, Name: cd.Name}, clusterSync); {
	case apierrors.IsNotFound(err):
		cdLog.Info("ClusterSync has not yet been created")
		status = corev1.ConditionTrue
		reason = "MissingClusterSync"
		message = "ClusterSync has not yet been created"
	case err != nil:
		cdLog.WithError(err).Log(controllerutils.LogLevel(err), "could not get ClusterSync")
		return err
	case checkForFailedSync(clusterSync):
		status = corev1.ConditionTrue
		reason = "SyncSetApplyFailure"
		message = "One of the SyncSet applies has failed"
	default:
		status = corev1.ConditionFalse
		reason = "SyncSetApplySuccess"
		message = "SyncSet apply is successful"
	}

	conds, changed := controllerutils.SetClusterDeploymentConditionWithChangeCheck(
		cd.Status.Conditions,
		hivev1.SyncSetFailedCondition,
		status,
		reason,
		message,
		controllerutils.UpdateConditionIfReasonOrMessageChange,
	)
	if !changed {
		return nil
	}
	cd.Status.Conditions = conds
	if err := r.Status().Update(context.TODO(), cd); err != nil {
		cdLog.WithError(err).Log(controllerutils.LogLevel(err), "error updating syncset failed condition")
		return err
	}
	return nil
}

// addOwnershipToSecret adds cluster deployment as an additional non-controlling owner to secret
func (r *ReconcileClusterDeployment) addOwnershipToSecret(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger, name string) error {
	cdLog = cdLog.WithField("secret", name)

	secret := &corev1.Secret{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: cd.Namespace, Name: name}, secret); err != nil {
		cdLog.WithError(err).Error("failed to get secret")
		return err
	}

	labelAdded := false

	// Add the label for cluster deployment for reconciling later, and add the owner reference
	if secret.Labels[constants.ClusterDeploymentNameLabel] != cd.Name {
		cdLog.Debug("Setting label on derived object")
		secret.Labels = k8slabels.AddLabel(secret.Labels, constants.ClusterDeploymentNameLabel, cd.Name)
		labelAdded = true
	}

	cdRef := metav1.OwnerReference{
		APIVersion:         cd.APIVersion,
		Kind:               cd.Kind,
		Name:               cd.Name,
		UID:                cd.UID,
		BlockOwnerDeletion: pointer.BoolPtr(true),
	}

	cdRefChanged := librarygocontroller.EnsureOwnerRef(secret, cdRef)
	if cdRefChanged {
		cdLog.Debug("ownership added for cluster deployment")
	}
	if cdRefChanged || labelAdded {
		cdLog.Info("secret has been modified, updating")
		if err := r.Update(context.TODO(), secret); err != nil {
			cdLog.WithError(err).Log(controllerutils.LogLevel(err), "error updating secret")
			return err
		}
	}
	return nil
}

// getClusterPlatform returns the platform of a given ClusterDeployment
func getClusterPlatform(cd *hivev1.ClusterDeployment) string {
	switch {
	case cd.Spec.Platform.AWS != nil:
		return constants.PlatformAWS
	case cd.Spec.Platform.Azure != nil:
		return constants.PlatformAzure
	case cd.Spec.Platform.GCP != nil:
		return constants.PlatformGCP
	case cd.Spec.Platform.OpenStack != nil:
		return constants.PlatformOpenStack
	case cd.Spec.Platform.VSphere != nil:
		return constants.PlatformVSphere
	case cd.Spec.Platform.BareMetal != nil:
		return constants.PlatformBaremetal
	case cd.Spec.Platform.AgentBareMetal != nil:
		return constants.PlatformAgentBaremetal
	}
	return constants.PlatformUnknown
}

// getClusterRegion returns the region of a given ClusterDeployment
func getClusterRegion(cd *hivev1.ClusterDeployment) string {
	switch {
	case cd.Spec.Platform.AWS != nil:
		return cd.Spec.Platform.AWS.Region
	case cd.Spec.Platform.Azure != nil:
		return cd.Spec.Platform.Azure.Region
	case cd.Spec.Platform.GCP != nil:
		return cd.Spec.Platform.GCP.Region
	}
	return regionUnknown
}
