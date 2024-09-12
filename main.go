package main

import (
	"context"
	"fmt"
	"log"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/privatedns/armprivatedns"
)

var (
	subscriptionID     = os.Getenv("AZURE_SUBSCRIPTION_ID")
	resourceGroupName  = os.Getenv("AZURE_RESOURCE_GROUP")
	privateDNSZoneName = os.Getenv("AZURE_PRIVATE_DNS_ZONE")
)

func main() {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
	})
	if err != nil {
		log.Fatalf("Unable to start manager: %v", err)
	}

	if err = (&ServiceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		log.Fatalf("Unable to create controller: %v", err)
	}

	log.Println("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Fatalf("Problem running manager: %v", err)
	}
}

// ServiceReconciler reconciles a Service object
type ServiceReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	DNSClient *armprivatedns.RecordSetsClient
}

// Reconcile updates the Azure Private DNS Zone based on the Service changes
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var svc corev1.Service
	if err := r.Get(ctx, req.NamespacedName, &svc); err != nil {
		if errors.IsNotFound(err) {
			// Service deleted; remove DNS record
			recordName := fmt.Sprintf("%s.%s", req.Name, req.Namespace)
			_, err = r.DNSClient.Delete(ctx, resourceGroupName, privateDNSZoneName, armprivatedns.RecordTypeA, recordName, nil)
			if err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Collect IP addresses from the Service
	var ipAddresses []string
	if svc.Spec.Type == corev1.ServiceTypeClusterIP {
		if svc.Spec.ClusterIP != "" && svc.Spec.ClusterIP != "None" {
			ipAddresses = append(ipAddresses, svc.Spec.ClusterIP)
		}
	} else if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
		for _, ingress := range svc.Status.LoadBalancer.Ingress {
			if ingress.IP != "" {
				ipAddresses = append(ipAddresses, ingress.IP)
			}
		}
	}

	if len(ipAddresses) == 0 {
		// No IPs to update; skip
		return ctrl.Result{}, nil
	}

	// Update Azure Private DNS
	recordName := fmt.Sprintf("%s.%s", svc.Name, svc.Namespace)
	ttl := int64(300)
	aRecords := make([]*armprivatedns.ARecord, len(ipAddresses))
	for i, ip := range ipAddresses {
		aRecords[i] = &armprivatedns.ARecord{IPv4Address: &ip}
	}
	aRecordSet := armprivatedns.RecordSet{
		Properties: &armprivatedns.RecordSetProperties{
			TTL:      &ttl,
			ARecords: aRecords,
		},
	}

	_, err := r.DNSClient.CreateOrUpdate(
		ctx,
		resourceGroupName,
		privateDNSZoneName,
		armprivatedns.RecordTypeA,
		recordName,
		aRecordSet,
		nil,
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager initializes the controller and sets up the Azure Private DNS client
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return err
	}
	dnsClient, err := armprivatedns.NewRecordSetsClient(subscriptionID, cred, nil)
	if err != nil {
		return err
	}
	r.DNSClient = dnsClient

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Complete(r)
}
