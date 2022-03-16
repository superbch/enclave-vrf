package main

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	secp256k1 "github.com/btcsuite/btcd/btcec"
	"github.com/edgelesssys/ego/ecrypto"
	"github.com/edgelesssys/ego/enclave"
	vrf "github.com/vechain/go-ecvrf"
)

// #include "util.h"
import "C"

type vrfResult struct {
	PI   string
	Beta string
}

var listenURL string
var peers []string
var peerUniqueIDs [][]byte
var vrfPubkey []byte //compressed pubkey
var vrfPrivKey *secp256k1.PrivateKey

var blockHash2Beta map[string][]byte = make(map[string][]byte)
var blockHash2PI map[string][]byte = make(map[string][]byte)
var blockHash2Timestamp map[string]uint64 = make(map[string]uint64)
var blockHashSet []string
var lock sync.RWMutex

var keyFile = "/data/key.txt"

var signer []byte
var isMaster bool

const serverName = "SGX-VRF-PUBKEY"
const attestationProviderURL = "https://shareduks.uks.attest.azure.net"

// start slave first, then start master to send key to them
// master must be sure slave is our own enclave app
// slave no need to be sure master is enclave app because the same vrf pubkey provided by all slave and master owned, it can check outside.
func main() {
	now := getTimestampFromTSC()
	fmt.Println(now)
	initConfig()
	recoveryPrivateKeyFromFile()
	go createAndStartHttpsServer()
	time.Sleep(time.Second)
	go peerHandshake()
	select {}
}

func initConfig() {
	isMasterP := flag.Bool("m", false, "is master or not")
	listenURLP := flag.String("l", "0.0.0.0:8081", "listen address")
	signerArg := flag.String("s", "", "signer ID")
	peerString := flag.String("p", "", "peer address list seperated by comma")
	peerUniqIDArgs := flag.String("u", "", "peer unique id seperated by comma")
	flag.Parse()
	isMaster = *isMasterP
	listenURL = *listenURLP
	fmt.Println(isMaster)
	fmt.Println(listenURL)
	// get peers
	if *peerString != "" {
		peers = strings.Split(*peerString, ",")
	}
	if *peerUniqIDArgs != "" {
		peerUniqueIDStrings := strings.Split(*peerUniqIDArgs, ",")
		for _, str := range peerUniqueIDStrings {
			id, err := hex.DecodeString(str)
			if err != nil {
				panic(err)
			}
			peerUniqueIDs = append(peerUniqueIDs, id)
		}
	}
	if len(peerUniqueIDs) != len(peers) {
		panic("number of peer not match number of uniqueID")
	}
	// get signer command line argument
	var err error
	signer, err = hex.DecodeString(*signerArg)
	if err != nil {
		panic(err)
	}
	if len(signer) == 0 {
		flag.Usage()
		return
	}
}

func peerHandshake() {
	for i, peer := range peers {
		verifyPeerAndSendKey(peer, peerUniqueIDs[i])
	}
}

func verifyPeerAndSendKey(peerAddress string, uniqID []byte) {
	url := "https://" + peerAddress

	// Get server certificate and its report. Skip TLS certificate verification because
	// the certificate is self-signed and we will verify it using the report instead.
	tlsConfig := &tls.Config{InsecureSkipVerify: true}

	var certStr string
	var reportStr string
	var certBytes []byte
	var reportBytes []byte
	var err error
	// waiting for peer start
	for len(certStr) == 0 {
		fmt.Printf("wating for peer:%s\n", peerAddress)
		certStr = string(httpGet(tlsConfig, url+"/cert"))
		reportStr = string(httpGet(tlsConfig, url+"/peer-report"))
		time.Sleep(5 * time.Second)
	}

	certBytes, err = hex.DecodeString(certStr)
	if err != nil {
		panic(err)
	}
	reportBytes, err = hex.DecodeString(reportStr)
	if err != nil {
		panic(err)
	}
	if err := verifyReport(reportBytes, certBytes, signer, uniqID); err != nil {
		panic(err)
	}
	fmt.Printf("verify peer:%s passed\n", peerAddress)

	// Create a TLS config that uses the server certificate as root
	// CA so that future connections to the server can be verified.
	if isMaster && len(vrfPubkey) != 0 {
		cert, _ := x509.ParseCertificate(certBytes)
		tlsConfig = &tls.Config{RootCAs: x509.NewCertPool(), ServerName: serverName}
		tlsConfig.RootCAs.AddCert(cert)

		httpGet(tlsConfig, url+fmt.Sprintf("/key?k=%s", hex.EncodeToString(vrfPrivKey.Serialize())))
		fmt.Printf("send key to peer:%s passed\n", peerAddress)
	}
}

func generateRandom64Bytes() []byte {
	var out []byte
	var x C.uint16_t
	var retry C.int = 1
	for i := 0; i < 64; i++ {
		C.rdrand_16(&x, retry)
		out = append(out, byte(x))
	}
	return out
}

func verifyReport(reportBytes, certBytes, signer, uniqID []byte) error {
	report, err := enclave.VerifyRemoteReport(reportBytes)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(certBytes)
	if !bytes.Equal(report.Data[:len(hash)], hash[:]) {
		return errors.New("report data does not match the certificate's hash")
	}
	if !bytes.Equal(report.UniqueID, uniqID) {
		return errors.New("invalid unique id")
	}
	if report.SecurityVersion < 2 {
		return errors.New("invalid security version")
	}
	if binary.LittleEndian.Uint16(report.ProductID) != 0x001 {
		return errors.New("invalid product")
	}
	if !bytes.Equal(report.SignerID, signer) {
		return errors.New("invalid signer")
	}
	if report.Debug {
		return errors.New("should not open debug")
	}
	return nil
}

// IntelCPUFreq sudo dmidecode -t processor | grep "Speed"
const intelCPUFreq = 4700_000000

// todo: modify this when running on SGX2 support enclave
func getTimestampFromTSC() uint64 {
	//cycleNumber := uint64(C.TSC())
	//return cycleNumber * intelCPUFreq
	return uint64(time.Now().Unix())
}

func createAndStartHttpsServer() {
	// Create a TLS config with a self-signed certificate and an embedded report.
	//tlsCfg, err := enclave.CreateAttestationServerTLSConfig()
	cert, priv := createCertificate()
	tlsCfg := tls.Config{
		Certificates: []tls.Certificate{
			{
				Certificate: [][]byte{cert},
				PrivateKey:  priv,
			},
		},
	}
	certHash := sha256.Sum256(cert)

	// init handler for remote attestation
	http.HandleFunc("/cert", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(hex.EncodeToString(cert))) })
	http.HandleFunc("/peer-report", func(w http.ResponseWriter, r *http.Request) {
		peerReport, err := enclave.GetRemoteReport(certHash[:])
		if err != nil {
			panic(err)
		}
		w.Write([]byte(hex.EncodeToString(peerReport)))
	})

	initVrfHttpHandlers()

	server := http.Server{Addr: listenURL, TLSConfig: &tlsCfg, ReadTimeout: 3 * time.Second, WriteTimeout: 5 * time.Second}
	fmt.Println("listening ...")
	err := server.ListenAndServeTLS("", "")
	fmt.Println(err)
}

func createCertificate() ([]byte, crypto.PrivateKey) {
	template := &x509.Certificate{
		SerialNumber: &big.Int{},
		Subject:      pkix.Name{CommonName: serverName},
		NotAfter:     time.Now().Add(10 * 365 * time.Hour), // 10 years
		DNSNames:     []string{serverName},
	}
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	cert, _ := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	return cert, priv
}

func initVrfHttpHandlers() {
	// look up secp256k1 pubkey
	http.HandleFunc("/pubkey", func(w http.ResponseWriter, r *http.Request) {
		if len(vrfPubkey) == 0 {
			return
		}
		w.Write([]byte(hex.EncodeToString(vrfPubkey)))
		return
	})

	// not check the block hash correctness
	http.HandleFunc("/blockhash", func(w http.ResponseWriter, r *http.Request) {
		if len(vrfPubkey) == 0 {
			return
		}
		hash := r.URL.Query()["b"]
		if len(hash) == 0 {
			return
		}
		blkHash := hash[0]
		lock.Lock()
		defer lock.Unlock()

		if blockHash2Timestamp[blkHash] != 0 {
			return
		}
		hashBytes, err := hex.DecodeString(blkHash)
		if err != nil {
			return
		}
		beta, pi, err := vrf.NewSecp256k1Sha256Tai().Prove((*ecdsa.PrivateKey)(vrfPrivKey), hashBytes)
		if err != nil {
			return
		}
		blockHash2Beta[blkHash] = beta
		blockHash2PI[blkHash] = pi
		blockHash2Timestamp[blkHash] = getTimestampFromTSC()
		blockHashSet = append(blockHashSet, blkHash)
		clearOldBlockHash()
		fmt.Printf("%v sent block hash %v\n", r.RemoteAddr, r.URL.Query()["b"])
	})

	http.HandleFunc("/vrf", func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("%v sent block hash to get vrf %v\n", r.RemoteAddr, r.URL.Query()["b"])
		hash := r.URL.Query()["b"]
		if len(hash) == 0 {
			return
		}
		blkHash := hash[0]
		lock.RLock()
		defer lock.RUnlock()

		vrfTimestamp := blockHash2Timestamp[blkHash]
		if vrfTimestamp == 0 {
			return
		}
		if vrfTimestamp+5 > getTimestampFromTSC() {
			return
		}
		res := vrfResult{
			PI:   hex.EncodeToString(blockHash2PI[blkHash]),
			Beta: hex.EncodeToString(blockHash2Beta[blkHash]),
		}
		out, _ := json.Marshal(res)
		w.Write(out)
		return
	})

	if !isMaster {
		http.HandleFunc("/key", func(w http.ResponseWriter, r *http.Request) {
			fmt.Printf("%v sent key to me %v\n", r.RemoteAddr, r.URL.Query()["k"])
			if len(vrfPubkey) != 0 {
				return
			}
			keys := r.URL.Query()["k"]
			if len(keys) == 0 {
				return
			}
			key := keys[0]
			keyBytes, err := hex.DecodeString(key)
			if err != nil {
				return
			}
			priv, pubkey := secp256k1.PrivKeyFromBytes(secp256k1.S256(), keyBytes)
			vrfPrivKey = priv
			vrfPubkey = pubkey.SerializeCompressed()

			fmt.Printf("enclave vrf private key from master:%s\n", hex.EncodeToString(vrfPrivKey.Serialize()))
			fmt.Printf("enclave vrf pubkey key from master:%s\n", hex.EncodeToString(vrfPubkey))
			sealKeyToFile()
			return
		})
	}

	http.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		if len(vrfPubkey) == 0 {
			return
		}
		hash := sha256.Sum256(vrfPubkey)
		report, err := enclave.GetRemoteReport(hash[:])
		if err != nil {
			return
		}
		w.Write([]byte(hex.EncodeToString(report)))
	})

	// send jwt token
	http.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if len(vrfPubkey) == 0 {
			return
		}
		token, err := enclave.CreateAzureAttestationToken(vrfPubkey, attestationProviderURL)
		if err != nil {
			return
		}
		w.Write([]byte(token))
	})
}

const blockHashEntryMax = 1000_000

func clearOldBlockHash() {
	nums := len(blockHashSet)
	if nums > blockHashEntryMax*1.5 {
		for _, bh := range blockHashSet[:nums-blockHashEntryMax] {
			delete(blockHash2Timestamp, bh)
			delete(blockHash2PI, bh)
			delete(blockHash2Beta, bh)
		}
		var tmpSet = make([]string, nums-blockHashEntryMax)
		copy(tmpSet, blockHashSet[nums-blockHashEntryMax:])
		blockHashSet = tmpSet
	}
}

func recoveryPrivateKeyFromFile() {
	fileData, err := os.ReadFile(keyFile)
	if err != nil {
		fmt.Printf("read file failed, %s\n", err.Error())
		if os.IsNotExist(err) {
			// maybe first run this enclave app
			if isMaster {
				generateVRFPrivateKey()
			}
		}
		return
	}
	rawData, err := ecrypto.Unseal(fileData, nil)
	if err != nil {
		fmt.Printf("unseal file data failed, %s\n", err.Error())
		return
	}
	vrfPrivKey, _ = secp256k1.PrivKeyFromBytes(secp256k1.S256(), rawData)
	vrfPubkey = vrfPrivKey.PubKey().SerializeCompressed()
	fmt.Printf("recover vrf keys, key:%s\n", hex.EncodeToString(vrfPrivKey.Serialize()))
}

func generateVRFPrivateKey() {
	priv, _ := secp256k1.PrivKeyFromBytes(secp256k1.S256(), generateRandom64Bytes())
	vrfPrivKey = priv
	vrfPubkey = vrfPrivKey.PubKey().SerializeCompressed()

	fmt.Printf("enclave vrf private key:%s\n", hex.EncodeToString(vrfPrivKey.Serialize()))
	fmt.Printf("enclave vrf pubkey key:%s\n", hex.EncodeToString(vrfPubkey))
	sealKeyToFile()
	return
}

func sealKeyToFile() {
	out, err := ecrypto.SealWithProductKey(vrfPrivKey.Serialize(), nil)
	if err != nil {
		panic(err)
	}
	err = os.WriteFile(keyFile, out, 0600)
	if err != nil {
		panic(err)
	}
}

func httpGet(tlsConfig *tls.Config, url string) []byte {
	client := http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}, Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Println(err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Println(resp.Status)
		return nil
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(err)
		return nil
	}
	return body
}