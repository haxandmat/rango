package main

import (
	"crypto/rsa"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"math/rand"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/openware/rango/pkg/amqp"
	"github.com/openware/rango/pkg/auth"
	"github.com/openware/rango/pkg/metrics"
	"github.com/openware/rango/pkg/routing"
)

var (
	wsAddr   = flag.String("ws-addr", "", "http service address")
	amqpAddr = flag.String("amqp-addr", "", "AMQP server address")
	pubKey   = flag.String("pubKey", "config/rsa-key.pub", "Path to public key")
	exName   = flag.String("exchange", "peatio.events.ranger", "Exchange name of upstream messages")

	orderExchange   = flag.String("order-exchange", "peatio.events.ranger", "Exchange name for order messages")
	orderRoutingKey = flag.String("order-routing-key", "peatio.order.new", "Routing key for order messages")
)

const prefix = "Bearer "

type httpHanlder func(w http.ResponseWriter, r *http.Request)

func token(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(string(authHeader), prefix) {
		return ""
	}

	return authHeader[len(prefix):]
}

func authHandler(h httpHanlder, key *rsa.PublicKey, mustAuth bool) httpHanlder {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, err := auth.ParseAndValidate(token(r), key)

		if err != nil && mustAuth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if err == nil {
			r.Header.Set("JwtUID", auth.UID)
			r.Header.Set("JwtRole", auth.Role)
		} else {
			r.Header.Del("JwtUID")
			r.Header.Del("JwtRole")
		}
		h(w, r)
		return
	}
}

func setupLogger() {
	logLevel, ok := os.LookupEnv("LOG_LEVEL")
	if ok {
		level, err := zerolog.ParseLevel(strings.ToLower(logLevel))
		if err != nil {
			panic(err)
		}

		zerolog.SetGlobalLevel(level)
		return
	}

	zerolog.SetGlobalLevel(zerolog.DebugLevel)
}

func getPublicKey() (pub *rsa.PublicKey, err error) {
	ks := auth.KeyStore{}
	encPem := os.Getenv("JWT_PUBLIC_KEY")

	if encPem != "" {
		ks.LoadPublicKeyFromString(encPem)
	} else {
		ks.LoadPublicKeyFromFile(*pubKey)
	}
	if err != nil {
		return nil, err
	}
	if ks.PublicKey == nil {
		return nil, fmt.Errorf("failed")
	}
	return ks.PublicKey, nil
}

func getEnv(name, value string) string {
	v := os.Getenv(name)
	if v == "" {
		return value
	}
	return v
}

func getAMQPConnectionURL() string {
	if *amqpAddr != "" {
		return *amqpAddr
	}

	user := getEnv("RABBITMQ_USER", "guest")
	pass := getEnv("RABBITMQ_PASSWORD", "guest")
	host := getEnv("RABBITMQ_HOST", "localhost")
	port := getEnv("RABBITMQ_PORT", "5672")

	return fmt.Sprintf("amqp://%s:%s@%s:%s", user, pass, host, port)
}

func getServerAddress() string {
	if *wsAddr != "" {
		return *wsAddr
	}
	host := getEnv("RANGER_HOST", "0.0.0.0")
	port := getEnv("RANGER_PORT", "8080")
	return fmt.Sprintf("%s:%s", host, port)
}

func getRBACConfig() map[string][]string {
	envs := os.Environ()

	rbacEnv := filterPrefixed("RANGO_RBAC_", envs)

	return envToMatrix(rbacEnv, "RANGO_RBAC_")
}

func envToMatrix(env []string, trimPrefix string) map[string][]string {
	matr := make(map[string][]string)

	for _, rec := range env {
		kv := strings.Split(rec, "=")
		key := strings.ToLower(strings.TrimPrefix(kv[0], trimPrefix))
		value := strings.Split(kv[1], ",")

		matr[key] = value
	}

	return matr
}

func filterPrefixed(prefix string, arr []string) []string {
	var res []string

	for _, rec := range arr {
		if strings.HasPrefix(rec, prefix) {
			res = append(res, rec)
		}
	}

	return res
}

func main() {
	flag.Parse()

	setupLogger()

	metrics.Enable()

	rbac := getRBACConfig()
	pub, err := getPublicKey()
	if err != nil {
		log.Error().Msgf("Loading public key failed: %s", err.Error())
		time.Sleep(2 * time.Second)
		return
	}

	rand.Seed(time.Now().UnixNano())
	qName := fmt.Sprintf("rango.instance.%d", rand.Int())
	mq, err := amqp.NewAMQPSession(getAMQPConnectionURL())
	if err != nil {
		log.Fatal().Msgf("creating new AMQP session failed: %s", err.Error())
		return
	}

	hub := routing.NewHub(rbac, mq, *orderExchange, *orderRoutingKey)

	err = mq.Stream(*exName, qName, hub.ReceiveMsg)
	defer mq.Close(qName)

	if err != nil {
		log.Fatal().Msgf("AMQP init failed: %s", err.Error())
		return
	}

	go hub.ListenWebsocketEvents()

	wsHandler := func(w http.ResponseWriter, r *http.Request) {
		routing.NewClient(hub, w, r)
	}

	http.HandleFunc("/private", authHandler(wsHandler, pub, true))
	http.HandleFunc("/public", authHandler(wsHandler, pub, false))
	http.HandleFunc("/", authHandler(wsHandler, pub, false))

	go http.ListenAndServe(":4242", promhttp.Handler())

	log.Printf("Listenning on %s", getServerAddress())
	err = http.ListenAndServe(getServerAddress(), nil)
	if err != nil {
		log.Fatal().Msg("ListenAndServe failed: " + err.Error())
	}
}
