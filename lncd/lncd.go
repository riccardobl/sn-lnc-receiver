package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"encoding/json"
	"net/http"
	"strconv"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/lightning-node-connect/mailbox"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/signal"
	"google.golang.org/grpc"
	"gopkg.in/macaroon.v2"
)

func getEnv(key string, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func getEnvAsInt(key string, defaultValue int) int {
	valueStr := getEnv(key, "")
	if value, err := strconv.Atoi(valueStr); err == nil {
		return value
	}
	return defaultValue
}

func getEnvAsDuration(key string, defaultValue time.Duration) time.Duration {
	valueStr := getEnv(key, "")
	if value, err := time.ParseDuration(valueStr); err == nil {
		return value
	}
	return defaultValue
}

func getEnvAsBool(key string, defaultValue bool) bool {
	valueStr := getEnv(key, "")
	if value, err := strconv.ParseBool(valueStr); err == nil {
		return value
	}
	return defaultValue
}

var (
	LNCD_TIMEOUT                  = getEnvAsDuration("LNCD_TIMEOUT", 5*time.Minute)
	LNCD_LIMIT_ACTIVE_CONNECTIONS = getEnvAsInt("LNCD_LIMIT_ACTIVE_CONNECTIONS", 210)
	LNCD_STATS_INTERVAL           = getEnvAsDuration("LNCD_STATS_INTERVAL", 1*time.Minute)
	LNCD_DEBUG                    = getEnvAsBool("LNCD_DEBUG", false)
	LNCD_PORT                     = getEnv("LNCD_PORT", "7167")
	LNCD_HOST                     = getEnv("LNCD_HOST", "0.0.0.0")
	LNCD_AUTH_TOKEN               = getEnv("LNCD_AUTH_TOKEN", "")
	LNCD_TLS_CERT_PATH            = getEnv("LNCD_TLS_CERT_PATH", "")
	LNCD_TLS_KEY_PATH             = getEnv("LNCD_TLS_KEY_PATH", "")
	LNCD_HEALTHCHECK_SERVICE_PORT = getEnv("LNCD_HEALTHCHECK_SERVICE_PORT", "7168")
	LNCD_HEALTHCHECK_SERVICE_HOST = getEnv("LNCD_HEALTHCHECK_SERVICE_HOST", "127.0.0.1")
)

// //////////////////////////////
// DEBUG LOGS for secrets
// Never turn this on in production or it will leak user
// secrets to the stdout, that is undesirable.
var UNSAFE_LOGS = getEnvAsBool("LNCD_DEV_UNSAFE_LOG", false)

////////////////////////////////

type ConnectionInfo struct {
	Mailbox       string
	PairingPhrase string
	LocalKey      string
	RemoteKey     string
	Status        string
	macaroon      *macaroon.Macaroon
}

type ConnectionKey struct {
	mailbox       string
	pairingPhrase string
}

type Action struct {
	method     string
	payload    string
	onError    func(error)
	onResponse func(ConnectionInfo, string)
}

type Connection struct {
	connInfo     ConnectionInfo
	actions      chan Action
	grpcClient   *grpc.ClientConn
	registry     map[string]func(context.Context, *grpc.ClientConn, string, func(string, error))
	pool         *ConnectionPool
	timeoutTimer *time.Timer
	perms        *PermissionManager
}

type ConnectionPool struct {
	connections map[ConnectionKey]*Connection
	mutex       sync.Mutex
}

type RpcRequest struct {
	Connection ConnectionInfo
	Method     string
	Payload    string
}

type RpcResponse struct {
	Connection ConnectionInfo
	Result     string
	err        error
	errCode    int
}

func NewConnectionPool() *ConnectionPool {
	return &ConnectionPool{
		connections: make(map[ConnectionKey]*Connection),
	}
}

func NewConnection(pool *ConnectionPool, info ConnectionInfo) (*Connection, error) {
	localPriv, remotePub, err := parseKeys(
		info.LocalKey, info.RemoteKey,
	)
	if err != nil {
		return nil, err
	}

	info.LocalKey = hex.EncodeToString(localPriv.Serialize())

	var ecdhPrivKey keychain.SingleKeyECDH = &keychain.PrivKeyECDH{PrivKey: localPriv}
	statusChecker, lndConnect, err := mailbox.NewClientWebsocketConn(
		info.Mailbox, info.PairingPhrase, ecdhPrivKey, remotePub,
		func(key *btcec.PublicKey) error {
			info.RemoteKey = hex.EncodeToString(key.SerializeCompressed())
			return nil
		}, func(data []byte) error {
			parts := strings.Split(string(data), ": ")
			if len(parts) != 2 || parts[0] != "Macaroon" {
				log.Errorf("authdata does not contain a macaroon")
				return errors.New("authdata does not contain a macaroon")
			}

			macBytes, err := hex.DecodeString(parts[1])
			if err != nil {
				return err
			}

			mac := &macaroon.Macaroon{}
			err = mac.UnmarshalBinary(macBytes)
			if err != nil {
				log.Errorf("unable to decode macaroon: %v", err)
				return err
			}

			info.macaroon = mac

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	var lndConn *grpc.ClientConn
	lndConn, err = lndConnect()
	if err != nil {
		return nil, err
	}

	var status = statusChecker().String()
	info.Status = status

	var connection *Connection = &Connection{
		connInfo:   info,
		actions:    make(chan Action, 1),
		grpcClient: lndConn,
		registry:   make(map[string]func(context.Context, *grpc.ClientConn, string, func(string, error))),
		pool:       pool,
	}

	connection.perms, err = NewPermissionManager(connection)
	if err != nil {
		return nil, err
	}

	lnrpc.RegisterLightningJSONCallbacks(connection.registry)
	return connection, nil
}

func (conn *Connection) runLoop() {
	for req := range conn.actions {
		if req.method == "checkPerms" {
			log.Debugf("Checking permissions for: %v", req.payload)
			perms := []string{}
			err := json.Unmarshal([]byte(req.payload), &perms)
			if err != nil {
				req.onError(err)
			} else {
				var valid []bool = make([]bool, len(perms))
				for i, perm := range perms {
					allowed, err := conn.perms.check(perm)
					if err != nil {
						log.Errorf("Error checking permission: %v", err)
						valid[i] = false
					} else {
						valid[i] = allowed
					}
				}

				result, err := json.Marshal(valid)
				if err != nil {
					req.onError(err)
				} else {
					req.onResponse(conn.connInfo, string(result))
				}

			}
		} else {
			var methodFunc, ok = conn.registry[req.method]
			if ok {
				log.Infof("Executing method: %v", req.method)
				if UNSAFE_LOGS {
					log.Debugf("Execution: %v %v %v", conn.connInfo, req.method, req.payload)
				}
				methodFunc(context.Background(), conn.grpcClient, req.payload, func(resultJSON string, err error) {
					if err != nil {
						req.onError(err)
					} else {
						req.onResponse(conn.connInfo, resultJSON)
					}
				})
			}
		}
	}
}

func (conn *Connection) Close() {
	close(conn.actions)
	conn.grpcClient.Close()
}

func (pool *ConnectionPool) execute(info ConnectionInfo, req Action) {
	try := func() bool {
		var key ConnectionKey = ConnectionKey{info.Mailbox, info.PairingPhrase}
		var err error
		connection, ok := pool.connections[key]
		if !ok {
			log.Infof("Creating new connection")
			if UNSAFE_LOGS {
				log.Debugf("Connection: %v", info)
			}
			if len(pool.connections) >= LNCD_LIMIT_ACTIVE_CONNECTIONS {
				req.onError(fmt.Errorf("too many active connections"))
				return true
			}
			connection, err = NewConnection(pool, info)
			if err != nil {
				req.onError(err)
			} else {
				connection.timeoutTimer = time.AfterFunc(LNCD_TIMEOUT, func() {
					pool.mutex.Lock()
					if len(connection.actions) == 0 {
						log.Infof("Closing idle connection %v", info.RemoteKey)
						if UNSAFE_LOGS {
							log.Debugf("Connection: %v", info)
						}
						connection.Close()
						delete(pool.connections, ConnectionKey{info.Mailbox, info.PairingPhrase})
					} else {
						connection.timeoutTimer.Reset(LNCD_TIMEOUT)
					}
					pool.mutex.Unlock()
				})
				pool.connections[key] = connection
				go connection.runLoop()
			}
		} else {
			log.Infof("Reusing existing connection")
			if UNSAFE_LOGS {
				log.Debugf("Connection: %v", info)
			}
		}
		connection.actions <- req
		return false
	}

	retry := true
	for retry {
		pool.mutex.Lock()
		retry = try()
		pool.mutex.Unlock()
		if retry {
			log.Infof("Retrying connection")
			time.Sleep(1 * time.Second)
		}
	}
}

func writeJSONError(w http.ResponseWriter, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func rpcHandler(pool *ConnectionPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request RpcRequest
		defer r.Body.Close()

		if r.Method != http.MethodPost {
			writeJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeJSONError(w, err.Error(), http.StatusBadRequest)
			return
		}

		log.Infof("Incoming RPC request: %v", request.Method)
		if UNSAFE_LOGS {
			log.Debugf("Full request: %v", request)
		}

		var waitResponse chan RpcResponse = make(chan RpcResponse)

		pool.execute(request.Connection, Action{
			method:  request.Method,
			payload: request.Payload,
			onError: func(err error) {
				waitResponse <- RpcResponse{err: err, errCode: http.StatusInternalServerError}
				close(waitResponse)
			},
			onResponse: func(info ConnectionInfo, result string) {
				log.Debugf("RPC response: %v", result)
				if UNSAFE_LOGS {
					log.Debugf("Connection: %v", info)
				}
				waitResponse <- RpcResponse{
					Connection: info,
					Result:     result,
					err:        nil,
					errCode:    http.StatusOK,
				}
				close(waitResponse)
			},
		})

		var resp RpcResponse = <-waitResponse
		if resp.err == nil {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				resp.err = err
				resp.errCode = http.StatusInternalServerError
			}
		}

		if resp.err != nil {
			writeJSONError(w, resp.err.Error(), resp.errCode)
		}
	}
}

func parseKeys(localPrivKey, remotePubKey string) (
	*btcec.PrivateKey, *btcec.PublicKey, error) {

	var (
		localStaticKey  *btcec.PrivateKey
		remoteStaticKey *btcec.PublicKey
	)
	switch {

	// This is a new session for which a local key has not yet been derived,
	// so we generate a new key.
	case localPrivKey == "" && remotePubKey == "":
		privKey, err := btcec.NewPrivateKey()
		if err != nil {
			return nil, nil, err
		}
		localStaticKey = privKey
		if UNSAFE_LOGS {
			log.Debugf("Generated new priv key: %v", hex.EncodeToString(privKey.Serialize()))
		}

	// A local private key has been provided, so parse it.
	case remotePubKey == "":
		privKeyByte, err := hex.DecodeString(localPrivKey)
		if err != nil {
			return nil, nil, err
		}
		privKey, _ := btcec.PrivKeyFromBytes(privKeyByte)
		localStaticKey = privKey
		if UNSAFE_LOGS {
			log.Debugf("Parsed local priv key: %v", hex.EncodeToString(privKey.Serialize()))
		}

	// Both local private key and remote public key have been provided,
	// so parse them both into the appropriate types.
	default:
		// Both local and remote are set.
		localPrivKeyBytes, err := hex.DecodeString(localPrivKey)
		if err != nil {
			return nil, nil, err
		}
		privKey, _ := btcec.PrivKeyFromBytes(localPrivKeyBytes)
		localStaticKey = privKey

		remoteKeyBytes, err := hex.DecodeString(remotePubKey)
		if err != nil {
			return nil, nil, err
		}

		remoteStaticKey, err = btcec.ParsePubKey(remoteKeyBytes)
		if err != nil {
			return nil, nil, err
		}

		if UNSAFE_LOGS {
			log.Debugf("Parsed local priv key: %v", hex.EncodeToString(privKey.Serialize()))
			log.Debugf("Parsed remote pub key: %v", hex.EncodeToString(remoteStaticKey.SerializeCompressed()))
		}
	}

	return localStaticKey, remoteStaticKey, nil
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if LNCD_AUTH_TOKEN != "" {
			authHeader := r.Header.Get("Authorization")
			if !strings.HasPrefix(authHeader, "Bearer ") {
				writeJSONError(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if token != LNCD_AUTH_TOKEN {
				writeJSONError(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	}
}

func main() {
	shutdownInterceptor, err := signal.Intercept()
	if err != nil {
		exit(err)
	}
	logWriter := build.NewRotatingLogWriter()
	SetupLoggers(logWriter, shutdownInterceptor, LNCD_DEBUG)

	log.Infof("Starting daemon")
	log.Infof("LNCD_TIMEOUT: %v", LNCD_TIMEOUT)
	log.Infof("LNCD_LIMIT_ACTIVE_CONNECTIONS: %v", LNCD_LIMIT_ACTIVE_CONNECTIONS)
	log.Infof("LNCD_STATS_INTERVAL: %v", LNCD_STATS_INTERVAL)
	log.Infof("LNCD_DEBUG: %v", LNCD_DEBUG)
	log.Infof("LNCD_PORT: %v", LNCD_PORT)
	log.Infof("LNCD_HOST: %v", LNCD_HOST)
	log.Infof("LNCD_TLS_CERT_PATH: %v", LNCD_TLS_CERT_PATH)
	log.Infof("LNCD_TLS_KEY_PATH: %v", LNCD_TLS_KEY_PATH)
	log.Infof("LNCD_HEALTHCHECK_SERVICE_PORT: %v", LNCD_HEALTHCHECK_SERVICE_PORT)
	log.Infof("LNCD_HEALTHCHECK_SERVICE_HOST: %v", LNCD_HEALTHCHECK_SERVICE_HOST)

	if UNSAFE_LOGS {
		log.Infof("LNCD_AUTH_TOKEN: %v", LNCD_AUTH_TOKEN)
		log.Infof("!!! UNSAFE LOGGING ENABLED !!!")
	}
	log.Debugf("debug enabled")

	var pool *ConnectionPool = NewConnectionPool()
	startStatsLoop(pool)

	http.HandleFunc("/rpc", authMiddleware(rpcHandler(pool)))
	http.HandleFunc("/health", authMiddleware(healthCheckHandler))
	http.HandleFunc("/", formHandler)

	go func() {
		log.Infof("Server starting at " + LNCD_HOST + ":" + LNCD_PORT)
		var isTLS = LNCD_TLS_CERT_PATH != "" && LNCD_TLS_KEY_PATH != ""
		if isTLS {
			log.Infof("TLS enabled")
			if err := http.ListenAndServeTLS(LNCD_HOST+":"+LNCD_PORT, LNCD_TLS_CERT_PATH, LNCD_TLS_KEY_PATH, nil); err != nil {
				log.Errorf("Error starting server: %v", err)
				exit(err)
			}
		} else {
			if err := http.ListenAndServe(LNCD_HOST+":"+LNCD_PORT, nil); err != nil {
				log.Errorf("Error starting server: %v", err)
				exit(err)
			}
		}
	}()

	if LNCD_HEALTHCHECK_SERVICE_HOST != "" && LNCD_HEALTHCHECK_SERVICE_PORT != "" {
		go func() {
			log.Infof("HealthCheck service starting at " + LNCD_HEALTHCHECK_SERVICE_HOST + ":" + LNCD_HEALTHCHECK_SERVICE_PORT)
			var rawHealthMux *http.ServeMux = http.NewServeMux()
			rawHealthMux.HandleFunc("/health", healthCheckHandler)
			if err := http.ListenAndServe(LNCD_HEALTHCHECK_SERVICE_HOST+":"+LNCD_HEALTHCHECK_SERVICE_PORT, rawHealthMux); err != nil {
				log.Errorf("Error starting HealthCheck server: %v", err)
				exit(err)
			}
		}()
	}

	<-shutdownInterceptor.ShutdownChannel()
	log.Infof("Shutting down daemon")
	for _, conn := range pool.connections {
		conn.Close()
	}
	log.Infof("Shutdown complete")

}

func exit(err error) {
	fmt.Printf("Error running daemon: %v\n", err)
	os.Exit(1)
}
