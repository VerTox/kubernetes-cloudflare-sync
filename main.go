package main

import (
	"context"
	"flag"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	core_v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

var options = struct {
	CloudflareAPIEmail string
	CloudflareAPIKey   string
	CloudflareAPIToken string
	CloudflareProxy    string
	CloudflareTTL      string
	DNSName            string
	DNSRoots           string
	UseInternalIP      bool
	SkipExternalIP     bool
	NodeSelector       string
}{
	CloudflareAPIEmail: os.Getenv("CF_API_EMAIL"),
	CloudflareAPIKey:   os.Getenv("CF_API_KEY"),
	CloudflareAPIToken: os.Getenv("CF_API_TOKEN"),
	CloudflareProxy:    os.Getenv("CF_PROXY"),
	CloudflareTTL:      os.Getenv("CF_TTL"),
	DNSName:            os.Getenv("DNS_NAME"),
	DNSRoots:           os.Getenv("DNS_ROOTS"),
	UseInternalIP:      os.Getenv("USE_INTERNAL_IP") != "",
	SkipExternalIP:     os.Getenv("SKIP_EXTERNAL_IP") != "",
	NodeSelector:       os.Getenv("NODE_SELECTOR"),
}

func main() {
	flag.StringVar(&options.DNSRoots, "dns-roots", options.DNSRoots, "the dns root domain, comma-seperated for multiple")
	flag.StringVar(&options.DNSName, "dns-name", options.DNSName, "the FQDN name for the nodes, comma-separated for multiple. Needs to be within one of the roots in DNSRoots")
	flag.StringVar(&options.CloudflareAPIEmail, "cloudflare-api-email", options.CloudflareAPIEmail, "the email address to use for cloudflare")
	flag.StringVar(&options.CloudflareAPIKey, "cloudflare-api-key", options.CloudflareAPIKey, "the key to use for cloudflare")
	flag.StringVar(&options.CloudflareAPIToken, "cloudflare-api-token", options.CloudflareAPIToken, "the token to use for cloudflare")
	flag.StringVar(&options.CloudflareProxy, "cloudflare-proxy", options.CloudflareProxy, "enable cloudflare proxy on dns (default false)")
	flag.StringVar(&options.CloudflareTTL, "cloudflare-ttl", options.CloudflareTTL, "ttl for dns (default 120)")
	flag.BoolVar(&options.UseInternalIP, "use-internal-ip", options.UseInternalIP, "use internal ips too if external ip's are not available")
	flag.BoolVar(&options.SkipExternalIP, "skip-external-ip", options.SkipExternalIP, "don't sync external IPs (use in conjunction with --use-internal-ip)")
	flag.StringVar(&options.NodeSelector, "node-selector", options.NodeSelector, "node selector query")
	flag.Parse()

	if options.CloudflareAPIToken == "" &&
		(options.CloudflareAPIEmail == "" || options.CloudflareAPIKey == "") {
		flag.Usage()
		log.Fatalln("cloudflare api token or email+key is required")
	}

	dnsNames := strings.Split(options.DNSName, ",")
	if len(dnsNames) == 1 && dnsNames[0] == "" {
		flag.Usage()
		log.Fatalln("dns name is required")
	}

	dnsRoots := strings.Split(options.DNSRoots, ",")
	if len(dnsRoots) == 1 && dnsRoots[0] == "" {
		flag.Usage()
		log.Fatalln("dns root is required")
	}

	cloudflareProxy, err := strconv.ParseBool(options.CloudflareProxy)
	if err != nil {
		log.Println("CloudflareProxy config not found or incorrect, defaulting to false")
		cloudflareProxy = false
	}

	cloudflareTTL, err := strconv.Atoi(options.CloudflareTTL)
	if err != nil {
		log.Println("CloudflareTTL config not found or incorrect, defaulting to 120")
		cloudflareTTL = 120
	}

	log.SetOutput(os.Stdout)

	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalln(err)
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalln(err)
	}

	stop := make(chan struct{})
	defer close(stop)

	nodeSelector := labels.NewSelector()
	if options.NodeSelector != "" {
		selector, err := labels.Parse(options.NodeSelector)
		if err != nil {
			log.Printf("node selector is invalid: %v\n", err)
		} else {
			nodeSelector = selector
		}
	}

	factory := informers.NewSharedInformerFactory(client, time.Minute)
	lister := factory.Core().V1().Nodes().Lister()
	var lastIPs []string
	resync := func() {
		log.Println("resyncing")
		nodes, err := lister.List(nodeSelector)
		if err != nil {
			log.Println("failed to list nodes", err)
		}

		var ips []string
		if !options.SkipExternalIP {
			for _, node := range nodes {
				if nodeIsReady(node) {
					for _, addr := range node.Status.Addresses {
						if addr.Type == core_v1.NodeExternalIP {
							ips = append(ips, addr.Address)
						}
					}
				}
			}
		}
		if options.UseInternalIP && len(ips) == 0 {
			for _, node := range nodes {
				if nodeIsReady(node) {
					for _, addr := range node.Status.Addresses {
						if addr.Type == core_v1.NodeInternalIP {
							ips = append(ips, addr.Address)
						}
					}
				}
			}
		}

		sort.Strings(ips)
		log.Println("ips:", ips)
		if strings.Join(ips, ",") == strings.Join(lastIPs, ",") {
			log.Println("no change detected")
			return
		}
		lastIPs = ips

		err = sync(context.Background(), ips, dnsNames, dnsRoots, cloudflareTTL, cloudflareProxy)
		if err != nil {
			log.Println("failed to sync", err)
		}
	}

	informer := factory.Core().V1().Nodes().Informer()
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			resync()
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			resync()
		},
		DeleteFunc: func(obj interface{}) {
			resync()
		},
	})
	informer.Run(stop)

	select {}
}

func nodeIsReady(node *core_v1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == core_v1.NodeReady && condition.Status == core_v1.ConditionTrue {
			return true
		}
	}

	return false
}
