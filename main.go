// Copyright 2021 Kevin Retzke
//
// This program is free software: you can redistribute it and/or modify it under
// the terms of the GNU Affero General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option) any
// later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS
// FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License for more
// details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.
package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v2"
)

var (
	config = flag.String("config", "config.yaml", "Config file dir")
)

var (
	Version = "1.0.0"
)

const (
	IsConn          = "conn"
	IsState         = "state"
	IsWalletSync    = "wallet-sync"
	IsWalletBalance = "wallet-balance"
	IsFarmed        = "farmed-amount"
	IsPlots         = "plots"
	IsPool          = "pool"
)

// yaml config struct representation
type (
	Config struct {
		Port  string          `yaml:"port"`
		Coins map[string]Coin `yaml:"coins"`
	}
	Coin struct {
		Cert          string          `yaml:"cert"`
		Key           string          `yaml:"key"`
		Host          string          `yaml:"host"`
		FullNodePort  string          `yaml:"full-node-port"`
		WalletPort    string          `yaml:"wallet-port"`
		FarmerPort    string          `yaml:"farmer-port"`
		HarvesterPort string          `yaml:"harvester-port"`
		PullSwitcher  map[string]bool `yaml:"pull-switcher"`
	}
)

func main() {
	log.Printf("chia_exporter_nforks version %s", Version)
	flag.Parse()

	f, err := os.Open(*config)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	cfgSrc, err := ioutil.ReadAll(f)
	if err != nil {
		log.Fatal(err)
	}

	config := Config{}
	err = yaml.Unmarshal(cfgSrc, &config)
	if err != nil {
		log.Fatal(err)
	}

	collectors := CoinsCollector{
		Collectors: make(map[string]Collector),
	}

	for name, coin := range config.Coins {
		coll := Collector{
			name:         name,
			baseURL:      fmt.Sprintf("%s:%s", coin.Host, coin.FullNodePort),
			walletURL:    fmt.Sprintf("%s:%s", coin.Host, coin.WalletPort),
			farmerURL:    fmt.Sprintf("%s:%s", coin.Host, coin.FarmerPort),
			harvesterURL: fmt.Sprintf("%s:%s", coin.Host, coin.HarvesterPort),
			pullSwitcher: coin.PullSwitcher,
		}

		client, err := newClient(os.ExpandEnv(coin.Cert), os.ExpandEnv(coin.Key))
		if err != nil {
			log.Fatal(name, err)
		}
		var info NetworkInfo
		if err := queryAPI(client, coll.baseURL, "get_network_info", "", &info); err != nil {
			log.Print(name, err)
		} else {
			log.Printf("[%s] Connected to node at %s on %s", name, coll.baseURL, info.NetworkName)
		}

		coll.client = client
		collectors.Collectors[name] = coll
	}

	prometheus.MustRegister(collectors)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "chia_exporter_nforks version %s\n", Version)
		fmt.Fprintf(w, "metrics are published on /metrics\n\n")
		fmt.Fprintf(w, "This program is free software released under the GNU AGPL.\n")
		fmt.Fprintf(w, "The source code is availabe at https://github.com/gusaul/chia_exporter_nforks\n")
	})
	http.Handle("/metrics", promhttp.Handler())

	addr := fmt.Sprintf(":%s", config.Port)
	log.Printf("Listening on %s. Serving metrics on /metrics.", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func newClient(cert, key string) (*http.Client, error) {
	c, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			TLSClientConfig: &tls.Config{
				Certificates:       []tls.Certificate{c},
				InsecureSkipVerify: true,
			},
		},
		Timeout: 5 * time.Second,
	}, nil
}

func queryAPI(client *http.Client, base, endpoint, query string, result interface{}) error {
	if query == "" {
		query = `{"":""}`
	}
	b := strings.NewReader(query)
	r, err := client.Post(base+"/"+endpoint, "application/json", b)
	if err != nil {
		return fmt.Errorf("error calling %s: %w", endpoint, err)
	}
	//t := io.TeeReader(r.Body, os.Stdout)
	t := io.TeeReader(r.Body, ioutil.Discard)
	if err := json.NewDecoder(t).Decode(result); err != nil {
		if err != nil {
			return fmt.Errorf("error decoding %s response: %w", endpoint, err)
		}
	}
	return nil
}

type CoinsCollector struct {
	Collectors map[string]Collector
}

type Collector struct {
	name         string
	client       *http.Client
	baseURL      string
	walletURL    string
	farmerURL    string
	harvesterURL string
	pullSwitcher map[string]bool
}

// Describe is implemented with DescribeByCollect.
func (cc CoinsCollector) Describe(ch chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(cc, ch)
}

// Collect queries Chia and returns metrics on ch.
func (cc CoinsCollector) Collect(ch chan<- prometheus.Metric) {
	var wg sync.WaitGroup
	for _, coll := range cc.Collectors {
		currColl := coll

		wg.Add(1)
		go func() {
			currColl.collectConnections(ch)
			wg.Done()
		}()

		wg.Add(1)
		go func() {
			currColl.collectBlockchainState(ch)
			wg.Done()
		}()

		wg.Add(1)
		go func() {
			currColl.collectWallets(ch)
			wg.Done()
		}()

		wg.Add(1)
		go func() {
			currColl.collectPoolState(ch)
			wg.Done()
		}()

		wg.Add(1)
		go func() {
			currColl.collectPlots(ch)
			wg.Done()
		}()
	}
	wg.Wait()
}

func (cc Collector) collectConnections(ch chan<- prometheus.Metric) {
	if !cc.pullSwitcher[IsConn] {
		return
	}

	var conns Connections
	if err := queryAPI(cc.client, cc.baseURL, "get_connections", "", &conns); err != nil {
		log.Print(err)
		return
	}
	peers := make([]int, NumNodeTypes)
	for _, p := range conns.Connections {
		peers[p.Type-1]++
	}
	desc := prometheus.NewDesc(
		fmt.Sprintf("%s_peers_count", cc.name),
		"Number of peers currently connected.",
		[]string{"type"}, nil,
	)
	for nt, cnt := range peers {
		ch <- prometheus.MustNewConstMetric(
			desc,
			prometheus.GaugeValue,
			float64(cnt),
			strconv.Itoa(nt+1),
		)
	}
}

func (cc Collector) collectBlockchainState(ch chan<- prometheus.Metric) {
	if !cc.pullSwitcher[IsState] {
		return
	}

	var bs BlockchainState
	if err := queryAPI(cc.client, cc.baseURL, "get_blockchain_state", "", &bs); err != nil {
		log.Print(err)
		return
	}
	sync := 0.0
	if bs.BlockchainState.Sync.SyncMode {
		sync = 1.0
	} else if bs.BlockchainState.Sync.Synced {
		sync = 2.0
	}
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			fmt.Sprintf("%s_blockchain_sync_status", cc.name),
			"Sync status, 0=not synced, 1=syncing, 2=synced",
			nil, nil,
		),
		prometheus.GaugeValue,
		sync,
	)
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			fmt.Sprintf("%s_blockchain_height", cc.name),
			"Current height",
			nil, nil,
		),
		prometheus.GaugeValue,
		float64(bs.BlockchainState.Peak.Height),
	)
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			fmt.Sprintf("%s_blockchain_difficulty", cc.name),
			"Current difficulty",
			nil, nil,
		),
		prometheus.GaugeValue,
		float64(bs.BlockchainState.Difficulty),
	)
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			fmt.Sprintf("%s_blockchain_space_bytes", cc.name),
			"Estimated current netspace",
			nil, nil,
		),
		prometheus.GaugeValue,
		bs.BlockchainState.Space,
	)
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			fmt.Sprintf("%s_blockchain_total_iters", cc.name),
			"Current total iterations",
			nil, nil,
		),
		prometheus.GaugeValue,
		float64(bs.BlockchainState.Peak.TotalIters),
	)
}

func (cc Collector) collectWallets(ch chan<- prometheus.Metric) {
	if !cc.pullSwitcher[IsWalletBalance] && !cc.pullSwitcher[IsWalletSync] && !cc.pullSwitcher[IsFarmed] {
		return
	}

	var ws Wallets
	if err := queryAPI(cc.client, cc.walletURL, "get_wallets", "", &ws); err != nil {
		log.Print(err)
		return
	}
	for _, w := range ws.Wallets {
		w.StringID = strconv.Itoa(w.ID)
		w.PublicKey = cc.getWalletPublicKey(w)
		if cc.pullSwitcher[IsWalletBalance] {
			cc.collectWalletBalance(ch, w)
		}
		if cc.pullSwitcher[IsWalletSync] {
			cc.collectWalletSync(ch, w)
		}
		if cc.pullSwitcher[IsFarmed] {
			cc.collectFarmedAmount(ch, w)
		}
	}
}

// getWalletPublicKey returns the fingerprint of first public key associated
// with the wallet.
func (cc Collector) getWalletPublicKey(w Wallet) string {
	var wpks WalletPublicKeys
	q := fmt.Sprintf(`{"wallet_id":%d}`, w.ID)
	if err := queryAPI(cc.client, cc.walletURL, "get_public_keys", q, &wpks); err != nil {
		log.Print(err)
		return ""
	}
	if len(wpks.PublicKeyFingerprints) < 1 {
		log.Print("no public key")
		return ""
	}
	if len(wpks.PublicKeyFingerprints) > 1 {
		log.Print("more than one public key; returning first")
	}
	return strconv.Itoa(wpks.PublicKeyFingerprints[0])
}

var (
	confirmedBalanceDesc = func(name string) *prometheus.Desc {
		return prometheus.NewDesc(
			fmt.Sprintf("%s_wallet_confirmed_balance_mojo", name),
			"Confirmed wallet balance.",
			[]string{"wallet_id", "wallet_fingerprint"}, nil,
		)
	}
	unconfirmedBalanceDesc = func(name string) *prometheus.Desc {
		return prometheus.NewDesc(
			fmt.Sprintf("%s_wallet_unconfirmed_balance_mojo", name),
			"Unconfirmed wallet balance.",
			[]string{"wallet_id", "wallet_fingerprint"}, nil,
		)
	}
	spendableBalanceDesc = func(name string) *prometheus.Desc {
		return prometheus.NewDesc(
			fmt.Sprintf("%s_wallet_spendable_balance_mojo", name),
			"Spendable wallet balance.",
			[]string{"wallet_id", "wallet_fingerprint"}, nil,
		)
	}
	maxSendDesc = func(name string) *prometheus.Desc {
		return prometheus.NewDesc(
			fmt.Sprintf("%s_wallet_max_send_mojo", name),
			"Maximum sendable amount.",
			[]string{"wallet_id", "wallet_fingerprint"}, nil,
		)
	}
	pendingChangeDesc = func(name string) *prometheus.Desc {
		return prometheus.NewDesc(
			fmt.Sprintf("%s_wallet_pending_change_mojo", name),
			"Pending change amount.",
			[]string{"wallet_id", "wallet_fingerprint"}, nil,
		)
	}
)

func (cc Collector) collectWalletBalance(ch chan<- prometheus.Metric, w Wallet) {
	var wb WalletBalance
	q := fmt.Sprintf(`{"wallet_id":%d}`, w.ID)
	if err := queryAPI(cc.client, cc.walletURL, "get_wallet_balance", q, &wb); err != nil {
		log.Print(err)
		return
	}
	ch <- prometheus.MustNewConstMetric(
		confirmedBalanceDesc(cc.name),
		prometheus.GaugeValue,
		float64(wb.WalletBalance.ConfirmedBalance),
		w.StringID, w.PublicKey,
	)
	ch <- prometheus.MustNewConstMetric(
		unconfirmedBalanceDesc(cc.name),
		prometheus.GaugeValue,
		float64(wb.WalletBalance.UnconfirmedBalance),
		w.StringID, w.PublicKey,
	)
	ch <- prometheus.MustNewConstMetric(
		spendableBalanceDesc(cc.name),
		prometheus.GaugeValue,
		float64(wb.WalletBalance.SpendableBalance),
		w.StringID, w.PublicKey,
	)
	ch <- prometheus.MustNewConstMetric(
		maxSendDesc(cc.name),
		prometheus.GaugeValue,
		float64(wb.WalletBalance.MaxSendAmount),
		w.StringID, w.PublicKey,
	)
	ch <- prometheus.MustNewConstMetric(
		pendingChangeDesc(cc.name),
		prometheus.GaugeValue,
		float64(wb.WalletBalance.PendingChange),
		w.StringID, w.PublicKey,
	)
}

var (
	walletSyncStatusDesc = func(name string) *prometheus.Desc {
		return prometheus.NewDesc(
			fmt.Sprintf("%s_wallet_sync_status", name),
			"Sync status, 0=not synced, 1=syncing, 2=synced",
			[]string{"wallet_id", "wallet_fingerprint"}, nil,
		)
	}
	walletHeightDesc = func(name string) *prometheus.Desc {
		return prometheus.NewDesc(
			fmt.Sprintf("%s_wallet_height", name),
			"Wallet synced height.",
			[]string{"wallet_id", "wallet_fingerprint"}, nil,
		)
	}
)

func (cc Collector) collectWalletSync(ch chan<- prometheus.Metric, w Wallet) {
	var wss WalletSyncStatus
	q := fmt.Sprintf(`{"wallet_id":%d}`, w.ID)
	if err := queryAPI(cc.client, cc.walletURL, "get_sync_status", q, &wss); err != nil {
		log.Print(err)
		return
	}
	sync := 0.0
	if wss.Syncing {
		sync = 1.0
	} else if wss.Synced {
		sync = 2.0
	}
	ch <- prometheus.MustNewConstMetric(
		walletSyncStatusDesc(cc.name),
		prometheus.GaugeValue,
		sync,
		w.StringID, w.PublicKey,
	)

	var whi WalletHeightInfo
	if err := queryAPI(cc.client, cc.walletURL, "get_height_info", q, &whi); err != nil {
		log.Print(err)
		return
	}
	ch <- prometheus.MustNewConstMetric(
		walletHeightDesc(cc.name),
		prometheus.GaugeValue,
		float64(whi.Height),
		w.StringID, w.PublicKey,
	)
}

func (cc Collector) collectPoolState(ch chan<- prometheus.Metric) {
	if !cc.pullSwitcher[IsPool] {
		return
	}

	var pools PoolState
	if err := queryAPI(cc.client, cc.farmerURL, "get_pool_state", "", &pools); err != nil {
		log.Print(err)
		return
	}
	for _, p := range pools.PoolState {
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				fmt.Sprintf("%s_pool_current_difficulty", cc.name),
				"Current difficulty on pool.",
				[]string{"launcher_id", "pool_url"}, nil,
			),
			prometheus.GaugeValue,
			float64(p.CurrentDificulty),
			p.PoolConfig.LauncherId,
			p.PoolConfig.PoolURL,
		)
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				fmt.Sprintf("%s_pool_current_points", cc.name),
				"Current points on pool.",
				[]string{"launcher_id", "pool_url"}, nil,
			),
			prometheus.GaugeValue,
			float64(p.CurrentPoints),
			p.PoolConfig.LauncherId,
			p.PoolConfig.PoolURL,
		)
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				fmt.Sprintf("%s_pool_points_acknowledged_24h", cc.name),
				"Points acknowledged last 24h on pool.",
				[]string{"launcher_id", "pool_url"}, nil,
			),
			prometheus.GaugeValue,
			float64(len(p.PointsAcknowledged24h)),
			p.PoolConfig.LauncherId,
			p.PoolConfig.PoolURL,
		)
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				fmt.Sprintf("%s_pool_points_found_24h", cc.name),
				"Points found last 24h on pool.",
				[]string{"launcher_id", "pool_url"}, nil,
			),
			prometheus.GaugeValue,
			float64(len(p.PointsFound24h)),
			p.PoolConfig.LauncherId,
			p.PoolConfig.PoolURL,
		)
	}
}

func (cc Collector) collectPlots(ch chan<- prometheus.Metric) {
	if !cc.pullSwitcher[IsPlots] {
		return
	}

	var plots PlotFiles
	if err := queryAPI(cc.client, cc.harvesterURL, "get_plots", "", &plots); err != nil {
		log.Print(err)
		return
	}
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			fmt.Sprintf("%s_plots_failed_to_open", cc.name),
			"Number of plots files failed to open.",
			nil, nil,
		),
		prometheus.GaugeValue,
		float64(len(plots.FailedToOpen)),
	)
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			fmt.Sprintf("%s_plots_not_found", cc.name),
			"Number of plots files not found.",
			nil, nil,
		),
		prometheus.GaugeValue,
		float64(len(plots.NotFound)),
	)
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			fmt.Sprintf("%s_plots", cc.name),
			"Number of plots currently using.",
			nil, nil,
		),
		prometheus.GaugeValue,
		float64(len(plots.Plots)),
	)
}

func (cc Collector) collectFarmedAmount(ch chan<- prometheus.Metric, w Wallet) {
	var farmed FarmedAmount
	q := fmt.Sprintf(`{"wallet_id":%d}`, w.ID)
	if err := queryAPI(cc.client, cc.walletURL, "get_farmed_amount", q, &farmed); err != nil {
		log.Print(err)
		return
	}
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			fmt.Sprintf("%s_wallet_farmed_amount", cc.name),
			"Farmed amount",
			[]string{"wallet_id", "wallet_fingerprint"}, nil,
		),
		prometheus.GaugeValue,
		float64(farmed.FarmedAmount),
		w.StringID, w.PublicKey,
	)
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			fmt.Sprintf("%s_wallet_reward_amount", cc.name),
			"Reward amount",
			[]string{"wallet_id", "wallet_fingerprint"}, nil,
		),
		prometheus.GaugeValue,
		float64(farmed.RewardAmount),
		w.StringID, w.PublicKey,
	)
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			fmt.Sprintf("%s_wallet_fee_amount", cc.name),
			"Fee amount amount",
			[]string{"wallet_id", "wallet_fingerprint"}, nil,
		),
		prometheus.GaugeValue,
		float64(farmed.FeeAmount),
		w.StringID, w.PublicKey,
	)
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			fmt.Sprintf("%s_wallet_last_height_farmed", cc.name),
			"Last height farmed",
			[]string{"wallet_id", "wallet_fingerprint"}, nil,
		),
		prometheus.GaugeValue,
		float64(farmed.LastHeightFarmed),
		w.StringID, w.PublicKey,
	)
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			fmt.Sprintf("%s_wallet_pool_reward_amount", cc.name),
			"Pool Reward amount",
			[]string{"wallet_id", "wallet_fingerprint"}, nil,
		),
		prometheus.GaugeValue,
		float64(farmed.PoolRewardAmount),
		w.StringID, w.PublicKey,
	)
}
