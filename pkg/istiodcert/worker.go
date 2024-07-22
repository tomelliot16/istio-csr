/*
Copyright 2024 The cert-manager Authors.

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

package istiodcert

import (
	"context"
	"fmt"
	"sync"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	cmversioned "github.com/cert-manager/cert-manager/pkg/client/clientset/versioned"
	cmclient "github.com/cert-manager/cert-manager/pkg/client/clientset/versioned/typed/certmanager/v1"
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/cert-manager/istio-csr/pkg/certmanager"
)

// DynamicIstiodCertProvisioner is both:
// 1. A controller-runtime controller for watching the dynamic istiod cert and keeping it updated
// 2. A wrapper around ctrlmgr.Runnable for listening for issuer changes and notifying the certificate controller
type DynamicIstiodCertProvisioner struct {
	log               logr.Logger
	certManagerClient cmclient.CertificateInterface
	opts              Options

	initialIssuerRef *cmmeta.ObjectReference
	issuerRef        *cmmeta.ObjectReference

	issuerRefMutex sync.Mutex

	issuerChangeChan <-chan *cmmeta.ObjectReference

	reconcileChan chan event.GenericEvent

	trustDomain string
}

// New creates a DynamicIstiodCertProvisioner, ready to be added to a controller manager
func New(log logr.Logger, restConfig *rest.Config, opts Options, issuerChangeNotifier certmanager.IssuerChangeNotifier, trustDomain string) (*DynamicIstiodCertProvisioner, error) {
	cmClient, err := cmversioned.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to build cert-manager client: %s", err)
	}

	initialIssuerRef := issuerChangeNotifier.InitialIssuer()

	return &DynamicIstiodCertProvisioner{
		log:               log,
		certManagerClient: cmClient.CertmanagerV1().Certificates(opts.CertificateNamespace),
		opts:              opts,

		initialIssuerRef: initialIssuerRef,
		issuerRef:        initialIssuerRef,

		issuerRefMutex: sync.Mutex{},

		issuerChangeChan: issuerChangeNotifier.SubscribeIssuerChange(),

		reconcileChan: make(chan event.GenericEvent),

		trustDomain: trustDomain,
	}, nil
}

// Start makes DynamicIstiodCertProvisioner a Runnable which can be invoked by a manager.
// It waits for a notification of an issuer change, and when it gets one it
// triggers reconciliation of the dynamic istiod cert.
func (dicp *DynamicIstiodCertProvisioner) Start(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			dicp.log.Info("Received context cancellation, shutting down dynamic istiod cert provisioner")
			return nil

		case newIssuer := <-dicp.issuerChangeChan:
			dicp.handleNewIssuer(newIssuer)
		}
	}
}

// NeedLeaderElection returns true, because the DynamicIstiodCertProvisioner should only run in one pod
// to avoid multiple pods trying to change the same certificate.
func (dicp *DynamicIstiodCertProvisioner) NeedLeaderElection() bool {
	return true
}

func (dicp *DynamicIstiodCertProvisioner) handleNewIssuer(issuerRef *cmmeta.ObjectReference) {
	dicp.issuerRefMutex.Lock()
	defer dicp.issuerRefMutex.Unlock()

	if issuerRef == nil && dicp.initialIssuerRef != nil {
		// don't blank out the issuer if there's an initial ref; use that instead
		dicp.issuerRef = dicp.initialIssuerRef
		return
	}

	dicp.issuerRef = issuerRef

	dicp.log.Info("triggering reconciliation of istiod cert after issuer change", "cert_name", dicp.opts.CertificateName, "cert_namespace", dicp.opts.CertificateNamespace)

	dicp.reconcileChan <- event.GenericEvent{
		Object: &cmapi.Certificate{
			ObjectMeta: metav1.ObjectMeta{
				Name:      dicp.opts.CertificateName,
				Namespace: dicp.opts.CertificateNamespace,
			},
		},
	}
}

// AddControllersToManager adds controllers to the given manager which:
// 1. Handle provisioning and updating the dynamic istiod cert
// 2. Handle listening for updates to the active issuer ref and re-issuing
func (dicp *DynamicIstiodCertProvisioner) AddControllersToManager(mgr manager.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr)

	b.For(
		new(cmapi.Certificate), builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			// Only process one specific cert which was requested
			return obj.GetName() == dicp.opts.CertificateName && obj.GetNamespace() == dicp.opts.CertificateNamespace
		})))

	// when the issuer changes, trigger a re-reconciliation
	b.WatchesRawSource(source.Channel(dicp.reconcileChan, handler.EnqueueRequestsFromMapFunc(
		func(context.Context, client.Object) []reconcile.Request {
			return []reconcile.Request{ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      dicp.opts.CertificateName,
					Namespace: dicp.opts.CertificateNamespace,
				},
			}}
		})))

	err := b.Complete(dicp)
	if err != nil {
		return err
	}

	err = mgr.Add(dicp)
	if err != nil {
		return err
	}

	return nil
}

func (dicp *DynamicIstiodCertProvisioner) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	dicp.issuerRefMutex.Lock()
	defer dicp.issuerRefMutex.Unlock()

	if dicp.issuerRef == nil {
		dicp.log.Info("exiting reconcile of dynamic istiod early; no issuerRef is set")
		return ctrl.Result{}, nil
	}

	dicp.log.Info("reconciling dynamic istiod cert", "issuer-name", dicp.issuerRef.Name, "issuer-kind", dicp.issuerRef.Kind, "issuer-group", dicp.issuerRef.Group)

	spiffeID := fmt.Sprintf("spiffe://%s/ns/%s/sa/istiod-service-account", dicp.trustDomain, req.Namespace)

	commonName, dnsNames := makeDNSNamesFromRevisions(req.Namespace, dicp.opts.IstioRevisions)

	if len(dicp.opts.AdditionalDNSNames) > 0 {
		dnsNames = append(dnsNames, dicp.opts.AdditionalDNSNames...)
	}

	desiredSpec := cmapi.CertificateSpec{
		CommonName:  commonName,
		DNSNames:    dnsNames,
		URIs:        []string{spiffeID},
		SecretName:  "istiod-tls",
		Duration:    &metav1.Duration{Duration: dicp.opts.Duration},
		RenewBefore: &metav1.Duration{Duration: dicp.opts.RenewBefore},
		PrivateKey: &cmapi.CertificatePrivateKey{
			RotationPolicy: cmapi.RotationPolicyAlways,
			Algorithm:      dicp.opts.CMKeyAlgorithm,
			Size:           dicp.opts.KeySize,
		},
		RevisionHistoryLimit: ptr.To(int32(1)),
		IssuerRef:            *dicp.issuerRef,
	}

	cert, err := dicp.certManagerClient.Get(ctx, req.Name, metav1.GetOptions{})

	if err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to fetch cert: %s", err)
		}

		cert := cmapi.Certificate{
			ObjectMeta: metav1.ObjectMeta{
				Name:      req.Name,
				Namespace: req.Namespace,
			},
			Spec: desiredSpec,
		}

		_, err = dicp.certManagerClient.Create(ctx, &cert, metav1.CreateOptions{})
		return ctrl.Result{}, err
	}

	cert.Spec = desiredSpec

	_, err = dicp.certManagerClient.Update(ctx, cert, metav1.UpdateOptions{})
	return ctrl.Result{}, err
}

// makeDNSNamesFromRevisions takes a list of istio revisions and produces a list of
// corresponding DNS names for the istiod cert, as well as returning the value for the common name
func makeDNSNamesFromRevisions(namespace string, istioRevisions []string) (string, []string) {
	if len(istioRevisions) == 0 {
		// It's unlikely to have no revisisions since the default is to have a
		// list of revisions containing just "default".
		// In any case, treat an empty list of revisions as being the same as just
		// the default revision.
		istioRevisions = []string{"default"}
	}

	var dnsNames []string

	// The default revision is a special case, and "default" isn't added to the DNS SAN, appearing as simply
	// istiod.<namespace>.svc
	defaultSAN := fmt.Sprintf("istiod.%s.svc", namespace)

	for _, revision := range istioRevisions {
		if revision == "default" {
			dnsNames = append(dnsNames, defaultSAN)
			continue
		}

		dnsNames = append(dnsNames, fmt.Sprintf("istiod%s.%s.svc", revision, namespace))
	}

	// Always return the default SAN as the commonName to match the behaviour of the static istiod cert
	// defined in the Helm chart.

	// It seems likely that we could do better here in the future and pick a commonName from the list of SANs we
	// created from the list of revisions, since there's a warning in the default values.yaml file about this:

	// > The common name for the istiod certificate is hard coded to the `default` revision DNS name.
	// > Some issuers may require that the common name on certificates match one
	// > of the DNS names. If:
	// > 1. Your issuer has this constraint, and
	// > 2. You are not using `default` as a revision,
	// > add the `default` revision here anyway. The resulting certificate will include a DNS name that won't be
	// > used, but will pass this constraint.

	return defaultSAN, dnsNames
}
