package main

import (
	"context"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"math/rand"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/privatedns/armprivatedns"
	"github.com/Azure/go-autorest/autorest/to"
)

type CustomTokenCredential struct {
	token string
}

func (c *CustomTokenCredential) GetToken(ctx context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{
		Token:     c.token,
		ExpiresOn: time.Now().Add(1 * time.Hour), // Set an appropriate expiration time
	}, nil
}

// NewCustomTokenCredential creates a new CustomTokenCredential
func NewCustomTokenCredential(token string) *CustomTokenCredential {
	return &CustomTokenCredential{token: token}
}

const (
	letters        = "abcdefghijklmnopqrstuvwxyz"
	hostnameLength = 10
)

func generateRandomHostname() string {
	rand.Seed(time.Now().UnixNano())
	var sb strings.Builder
	for i := 0; i < hostnameLength; i++ {
		sb.WriteByte(letters[rand.Intn(len(letters))])
	}
	return sb.String()
}

func main() {

	var cred azcore.TokenCredential
	if token, found := os.LookupEnv("ACCESS_TOKEN"); found {
		cred = NewCustomTokenCredential(token)
	} else {

		var err error
		cred, err = azidentity.NewAzureCLICredential(nil)
		if err != nil {
			log.Fatal(err.Error())
		}
	}
	options := arm.ClientOptions{}
	clientFactory, err := armprivatedns.NewClientFactory("8ecadfc9-d1a3-4ea4-b844-0d9f87e4d7c8", cred, &options)
	if err != nil {
		log.Fatal(err.Error())
	}
	ctx := context.Background()

	zoneclient := clientFactory.NewPrivateZonesClient()
	location := "global"
	rg := "paultest"
	zone := "dnstest.cluster.local"

	poller, err := zoneclient.BeginCreateOrUpdate(ctx, rg, zone, armprivatedns.PrivateZone{
		Location:   &location,
		Properties: &armprivatedns.PrivateZoneProperties{},
	}, &armprivatedns.PrivateZonesClientBeginCreateOrUpdateOptions{})
	if err != nil {
		log.Fatal(err.Error())
	}
	_, err = poller.Poll(ctx)
	if err != nil {
		log.Fatal(err.Error())
	}
	//log.Printf("got %+v", resp)

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{
				Timeout: time.Second * 2,
			}
			// Specify the DNS server here
			return d.DialContext(ctx, network, "168.63.129.16:53") // Google's DNS server
		},
	}

	rc := clientFactory.NewRecordSetsClient()

	runs := 1000
	var semaphor = make(chan bool, 10)
	var results = make(chan time.Duration, runs)
	for count := 0; count < runs; count++ {
		go func() {
			semaphor <- true
			defer func() { <-semaphor }()
			recordname := generateRandomHostname()
			_, err := rc.CreateOrUpdate(ctx, rg, zone, armprivatedns.RecordTypeA, recordname, armprivatedns.RecordSet{
				Properties: &armprivatedns.RecordSetProperties{
					TTL: to.Int64Ptr(60),
					ARecords: []*armprivatedns.ARecord{
						{
							IPv4Address: to.StringPtr("10.0.1.4"),
						},
					},
				},
			}, &armprivatedns.RecordSetsClientCreateOrUpdateOptions{})
			if err != nil {
				log.Fatal(err.Error())
			}
			//log.Printf("got %s =  %+v from arm call", *recordresp.Name, *recordresp.Properties)

			start := time.Now()

			for i := 0; i < 30; i++ {
				resp, err := resolver.LookupHost(context.TODO(), recordname+"."+zone)
				if err != nil {
					if de, ok := err.(*net.DNSError); ok && de.IsNotFound {
						//log.Printf("Got nxrecord")
						time.Sleep(time.Second / 2)
						continue
					}
					log.Fatalf("failed to lookup host %s: %s", recordname, err)
				}
				latency := time.Since(start)
				log.Printf("got %s-> %v after %s", recordname, resp, latency)
				results <- latency
				break
			}
		}()
	}

	var total, max time.Duration
	for count := 0; count < runs; count++ {
		l := <-results
		total += l
		if l > max {
			max = l
		}
	}
	average := total / time.Duration(runs)
	log.Printf("Got averge %s and max %s", average, max)

}
