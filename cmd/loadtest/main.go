package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/any"
	api "github.com/osrg/gobgp/api"
	"github.com/osrg/gobgp/pkg/packet/bgp"
	gobgp "github.com/osrg/gobgp/pkg/server"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"net"
	"os"
	"sync"
	"time"
)

type BGPConfig struct {
	LocalASN             uint32            `json:"localasn"`
	LocalIPv4            string            `json:"localipV4"`
	Sessions             []Session        `json:"sessions"`
	LogLevel             string        `json:"loglevel"`
}
type Session struct {
	Neighbour net.IP `json:"neighbour"`
	ASN       uint32 `json:"asn"`
	Family    string `json:"family"`
}

type Entry struct {
	source       string
	sourcePrefix uint32

	target       string
	targetPrefix uint32

	maxbps       uint64
}
var flows []*Entry

var (
	Config    *BGPConfig
	ConfigMux = new(sync.RWMutex)
)

var wg sync.WaitGroup



var flagBatchSize = flag.Int("batchSize", 1000, "Amount of flowspec rules to add per batch")
var flagBatchCount = flag.Int("batchCount", 130, "Amount of rule batches")
var flagRemoveDelay = flag.Int("removeDelay", 20, "How long before the flowspec rules are removed after adding in seconds")
var flagBatchDelay = flag.Int("batchDelay", 30, "How long between adding batches in seconds")

func main(){
	LoadConfig("configs/config.json", &Config, ConfigMux)

	flag.Usage = func() {
		fmt.Print(
	`Program flow:
		-> Generates IP list to use
		-> Start BGP and connect to peer
		-> Start flow churn, batch size 1000, batch count 130
		-> Loop for each batch (in this case 130 batches)
			-> Add flowspec rules (in this case a batch size of 1000, will add 1000 of them)
			-> Queue batch removal, delay 20 seconds 
			-> sleep 30`)
		fmt.Print("\n Arguments:\n ")
		flag.PrintDefaults()
	}
	flag.Parse()
	log.SetFormatter(&log.TextFormatter{
		TimestampFormat:  "",
		DisableTimestamp: false,
		FieldMap:         nil,
		CallerPrettyfier: nil,

	})
	//log.SetReportCaller(true)
	switch Config.LogLevel {
	case "DEBUG":
		log.SetLevel(log.DebugLevel)
		break
	case "INFO":
		log.SetLevel(log.InfoLevel)
		break
	case "WARN":
		log.SetLevel(log.WarnLevel)
		break
	default:
		log.SetLevel(log.InfoLevel)
	}
	log.Info("Log Level:" + log.GetLevel().String())


	wg.Add(1)
	log.Info("Generating IP List")
	generateIPList()
	log.Info("Starting BGP Services")
	go StartBgp()
	time.Sleep(5 * time.Second)
	log.Info("Starting FLow Churn")
	go StartFlowChurn(*flagBatchSize,*flagBatchCount)
	wg.Wait()



}
func LoadConfig(file string, store interface{}, mux *sync.RWMutex) {
	mux.Lock()
	path, _ := os.Getwd()
	filePath := path + "/" + file
	log.Info("Loading Config: " + filePath)
	rawFile, err := os.Open(filePath)
	if err != nil {
		log.Panic("Failed to open `" + filePath + "` with error: " + err.Error())
	}
	byteValue, _ := ioutil.ReadAll(rawFile)
	err = json.Unmarshal(byteValue, store)
	if err != nil {
		log.Panic("Failed to read json of `" + filePath + "` with error:" + err.Error())
	}
	defer rawFile.Close()
	mux.Unlock()
}

func generateIPList(){


	// 198.18.0.0/15
	// Network:	198.18.0.0/15
	// Netmask:	255.254.0.0 = 15
	// Broadcast:	198.19.255.255
	//
	// 	Address space:	Benchmarking
	// HostMin:	198.18.0.1
	// HostMax:	198.19.255.254
	// 	Hosts/Net:	131070
	ips,_  := Hosts("198.18.0.0/15")
	for _, IP := range ips {
		flows = append(flows,	&Entry{
			source:       IP,
			sourcePrefix: 32,
			target:       "198.17.0.1",
			targetPrefix: 24,
			maxbps:       100,
		} )

	}
	log.WithFields(log.Fields{
		"count":len(flows),
	}).Info("Loading IPs")

}
//https://gist.github.com/kotakanbe/d3059af990252ba89a82
func Hosts(cidr string) ([]string, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}

	var ips []string
	for ip := ip.Mask(ipnet.Mask); ipnet.Contains(ip); inc(ip) {
		ips = append(ips, ip.String())
	}
	// remove network address and broadcast address
	return ips[1 : len(ips)-1], nil
}

func inc(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

var bgpServer  *gobgp.BgpServer

func StartBgp() {
	defer wg.Done()
	//Create BGP service
	bgpServer = gobgp.NewBgpServer()

	go bgpServer.Serve()

	//Set up basic configs
	if err := bgpServer.StartBgp(context.Background(), &api.StartBgpRequest{
		Global: &api.Global{
			As:         Config.LocalASN,
			RouterId:   Config.LocalIPv4,
			ListenPort: -1,
		},
	}); err != nil {
		log.Fatal(err)
	}
	for _, aSession := range Config.Sessions {
		log.WithFields(log.Fields{

			"localAsn":  Config.LocalASN,
			"remoteAsn": aSession.ASN,
			"neighbor":  aSession.Neighbour,
		}).Info("Creating Session")

		//Create session
		var peer *api.Peer

		peer = createPeer(aSession)

		//Add session to router
		if err := bgpServer.AddPeer(context.Background(), &api.AddPeerRequest{
			Peer: peer,
		});

			err != nil {
			log.Error(err)
		}



	}

	select {}

}


func createPeer(session Session) (peer *api.Peer) {

	desc := fmt.Sprintf("%d", session.Neighbour)
	peer = &api.Peer{
		Conf: &api.PeerConf{
			Description:     desc,
			NeighborAddress: session.Neighbour.String(),
			PeerAs:          session.ASN,
		},
		ApplyPolicy: &api.ApplyPolicy{
			ImportPolicy: &api.PolicyAssignment{
				DefaultAction: api.RouteAction_ACCEPT,
			},
			ExportPolicy: &api.PolicyAssignment{
				DefaultAction: api.RouteAction_REJECT,
			},
		},
		EbgpMultihop: &api.EbgpMultihop{
			Enabled:     true,
			MultihopTtl: 20,
		},
		AfiSafis: []*api.AfiSafi{

			{
				Config: &api.AfiSafiConfig{
					Family: &api.Family{
						Afi:  api.Family_AFI_IP,
						Safi: bgp.SAFI_UNICAST,
					},
					Enabled: true,
				},
			},
			{
				Config: &api.AfiSafiConfig{
					Family: &api.Family{
						Afi:  api.Family_AFI_IP,
						Safi: bgp.SAFI_FLOW_SPEC_UNICAST,
					},
					Enabled: true,
				},
			},
		},
		Timers: &api.Timers{
			Config: &api.TimersConfig{
				MinimumAdvertisementInterval: 10,
			},
			State: &api.TimersState{
				MinimumAdvertisementInterval: 10,
			},
		},
	}

	return
}


func addFlow(e *Entry) (err error) {

	//Create flow match rule
	destRule, _ := ptypes.MarshalAny(&api.FlowSpecIPPrefix{
		Type:      1, // Destination Prefix
		PrefixLen: e.targetPrefix,
		Prefix:    e.target,
	})
	log.WithFields(log.Fields{
		"Target": e.target,
		"TargetPre": e.targetPrefix,
		"Source": e.source,
		"SourcePre": e.sourcePrefix,
		"RateLimit": e.maxbps,

	}).Debug("Added")

	srcRule, _ := ptypes.MarshalAny(&api.FlowSpecIPPrefix{
		Type:      2, // Source Prefix
		PrefixLen: e.sourcePrefix,
		Prefix:    e.source,
	})

	a2, _ := ptypes.MarshalAny(&api.NextHopAttribute{
		NextHop: "0.0.0.0",
	})

	//Add rules to rule
	flowRules := []*any.Any{srcRule, destRule}

	//Add rule to routing interface
	nlri, _ := ptypes.MarshalAny(&api.FlowSpecNLRI{
		Rules: flowRules,
	})
	a1, _ := ptypes.MarshalAny(&api.OriginAttribute{
		Origin: 0,
	})

	communities := make([]*any.Any, 0, 1)
	rateLimitAttr, _ := ptypes.MarshalAny(&api.TrafficRateExtended{
		As:   0,
		Rate: float32(e.maxbps),
	})
	communities = append(communities, rateLimitAttr)
	extendedCommunities, _ := ptypes.MarshalAny(&api.ExtendedCommunitiesAttribute{
		Communities: communities,
	})

	attrs := []*any.Any{a1, a2, extendedCommunities}

	//add add path to routing table.
	_, err = bgpServer.AddPath(context.Background(), &api.AddPathRequest{
		Path: &api.Path{
			Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_FLOW_SPEC_UNICAST},
			Nlri:   nlri,
			Pattrs: attrs,
		},
	})
	if err != nil {
		log.Error(err)
	}

	return
}

func removeFlow(e *Entry) (err error) {

	//Create flow match rule
	destRule, _ := ptypes.MarshalAny(&api.FlowSpecIPPrefix{
		Type:      1, // Destination Prefix
		PrefixLen: e.targetPrefix,
		Prefix:    e.target,
	})
	srcRule, _ := ptypes.MarshalAny(&api.FlowSpecIPPrefix{
		Type:      2, // Source Prefix
		PrefixLen: e.sourcePrefix,
		Prefix:    e.source,
	})
	log.WithFields(log.Fields{
		"Target": e.target,
		"TargetPre": e.targetPrefix,
		"Source": e.source,
		"SourcePre": e.sourcePrefix,
	}).Debug("Remove")

	//Add rules to rule
	flowRules := []*any.Any{srcRule, destRule}

	//Add rule to routing interface
	nlri, _ := ptypes.MarshalAny(&api.FlowSpecNLRI{
		Rules: flowRules,
	})
	a1, _ := ptypes.MarshalAny(&api.OriginAttribute{
		Origin: 0,
	})
	a2, _ := ptypes.MarshalAny(&api.NextHopAttribute{
		NextHop: "0.0.0.0",
	})
	communities := make([]*any.Any, 0, 1)
	rateLimitAttr, _ := ptypes.MarshalAny(&api.TrafficRateExtended{
		As:   0,
		Rate: float32(e.maxbps),
	})
	communities = append(communities, rateLimitAttr)
	extendedCommunities, _ := ptypes.MarshalAny(&api.ExtendedCommunitiesAttribute{
		Communities: communities,
	})

	attrs := []*any.Any{a1, a2, extendedCommunities}

	//add add path to routing table.
	err = bgpServer.DeletePath(context.Background(), &api.DeletePathRequest {
		Path: &api.Path{
			Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_FLOW_SPEC_UNICAST},
			Nlri:   nlri,
			Pattrs: attrs,
		},
	})
	if err != nil {
		log.Error(err)
	}

	return

}


func StartFlowChurn(batchSize int, batchCount int){

	if batchCount * batchSize > 131000 {
		log.Panic("batchCount * batchSize > 131000 - Please lower either batchCount or BatchSize")
	}
	for true {

		for i := 0; i < batchCount; i++ {
			addBatch(i * batchSize, batchSize)
		}

		for i := 0; i < batchCount; i++ {
			go removeBatch(i * batchSize, batchSize)
		}
		log.WithFields(log.Fields{
			"Seconds":*flagBatchDelay,
		}).Info("Sleeping Between Batch")
		time.Sleep(time.Duration(*flagBatchDelay) *  time.Second)

	}

}

func addBatch(start int, count int){
	for i, flow := range flows{
		if i >= start {
			addFlow(flow)
		}
		if i >= count+start {
			log.WithFields(log.Fields{
				"start":start,
				"count":count,
			}).Info("Add Batch Complete")
			return
		}
	}

}
func removeBatch(start int, count int){
	time.Sleep(time.Duration(*flagRemoveDelay) *  time.Second)
	for i, flow := range flows{
		if i >= start {
			removeFlow(flow)
		}
		if i >= count+start {
			log.WithFields(log.Fields{
				"start":start,
				"count":count,
				"Delayed Seconds": *flagRemoveDelay,
			}).Warn("Remove Batch Complete")
			return
		}
	}

}