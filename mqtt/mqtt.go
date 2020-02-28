package mqtt

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	log "github.com/sirupsen/logrus"
	"github.com/underflow101/server/configuration"
	gostLog "github.com/underflow101/server/log"
	"github.com/underflow101/server/sensorthings/models"
)

var logger *log.Entry

// MQTT is the implementation of the MQTT client
type MQTT struct {
	host            string
	port            int
	prefix          string
	clientID        string
	sslEnabled      bool
	username        string
	password        string
	caCertPath      string
	clientCertPath  string
	privateKeyPath  string
	keepAliveSec    int
	pingTimeoutSec  int
	subscriptionQos byte
	persistent      bool
	order           bool
	connecting      bool
	disconnected    bool
	client          paho.Client
	verbose         bool
	api             *models.API
	connectToken    *paho.ConnectToken
}

func setupLogger(verbose bool) {
	l, err := gostLog.GetLoggerInstance()
	if err != nil {
		log.Error(err)
	}

	logger = l.WithFields(log.Fields{"package": "gost.server.mqtt"})

	if verbose {
		paho.ERROR = logger
		paho.CRITICAL = logger
		paho.WARN = logger
		paho.DEBUG = logger
	}
}

func (m *MQTT) getProtocol() string {
	if m.sslEnabled == true {
		return "ssl"
	} else {
		return "tcp"
	}
}

func initMQTTClientOptions(client *MQTT) (*paho.ClientOptions, error) {

	opts := paho.NewClientOptions() // uses defaults: https://godoc.org/github.com/eclipse/paho.mqtt.golang#NewClientOptions

	if client.username != "" {
		opts.SetUsername(client.username)
	}
	if client.password != "" {
		opts.SetPassword(client.password)
	}

	// TLS CONFIG
	tlsConfig := &tls.Config{}
	if client.caCertPath != "" {

		// Import trusted certificates from CAfile.pem.
		// Alternatively, manually add CA certificates to
		// default openssl CA bundle.
		tlsConfig.RootCAs = x509.NewCertPool()
		pemCerts, err := ioutil.ReadFile(client.caCertPath)
		if err == nil {
			tlsConfig.RootCAs.AppendCertsFromPEM(pemCerts)
		}
	}
	if client.clientCertPath != "" && client.privateKeyPath != "" {
		// Import client certificate/key pair
		cert, err := tls.LoadX509KeyPair(client.clientCertPath, client.privateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("error loading client keypair: %s", err)
		}
		// Just to print out the client certificate..
		cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return nil, fmt.Errorf("error parsing client certificate: %s", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	opts.AddBroker(fmt.Sprintf("%s://%s:%v", client.getProtocol(), client.host, client.port))
	opts.SetTLSConfig(tlsConfig)

	opts.SetClientID(client.clientID)
	opts.SetCleanSession(!client.persistent)
	opts.SetOrderMatters(client.order)
	opts.SetKeepAlive(time.Duration(client.keepAliveSec) * time.Second)
	opts.SetPingTimeout(time.Duration(client.pingTimeoutSec) * time.Second)
	opts.SetAutoReconnect(false)
	opts.SetConnectionLostHandler(client.connectionLostHandler)
	opts.SetOnConnectHandler(client.connectHandler)
	return opts, nil
}

// CreateMQTTClient creates a new MQTT client
func CreateMQTTClient(config configuration.MQTTConfig) models.MQTTClient {
	setupLogger(config.Verbose)

	mqttClient := &MQTT{
		host:            config.Host,
		port:            config.Port,
		prefix:          config.Prefix,
		clientID:        config.ClientID,
		subscriptionQos: config.SubscriptionQos,
		persistent:      config.Persistent,
		order:           config.Order,
		sslEnabled:      config.SSL,
		username:        config.Username,
		password:        config.Password,
		caCertPath:      config.CaCertFile,
		clientCertPath:  config.ClientCertFile,
		privateKeyPath:  config.PrivateKeyFile,
		keepAliveSec:    config.KeepAliveSec,
		pingTimeoutSec:  config.PingTimeoutSec,
	}

	opts, err := initMQTTClientOptions(mqttClient)
	if err != nil {
		logger.Errorf("unable to configure MQTT client: %s", err)
	}

	pahoClient := paho.NewClient(opts)
	mqttClient.client = pahoClient

	return mqttClient
}

// Start running the MQTT client
func (m *MQTT) Start(api *models.API) {
	m.api = api
	logger.Infof("Starting MQTT client on %s://%s:%v with Prefix:%v, Persistence:%v, OrderMatters:%v, KeepAlive:%v, PingTimeout:%v, QOS:%v",
		m.getProtocol(), m.host, m.port, m.prefix, m.persistent, m.order, m.keepAliveSec, m.pingTimeoutSec, m.subscriptionQos)
	m.connect()
}

// Stop the MQTT client
func (m *MQTT) Stop() {
	m.client.Disconnect(500)
}

func (m *MQTT) subscribe() {
	a := *m.api
	topics := *a.GetTopics(m.prefix)

	for _, t := range topics {
		topic := t
		logger.Infof("MQTT client subscribing to %s", topic.Path)

		if token := m.client.Subscribe(topic.Path, m.subscriptionQos, func(client paho.Client, msg paho.Message) {
			go topic.Handler(m.api, m.prefix, msg.Topic(), msg.Payload())
		}); token.Wait() && token.Error() != nil {
			logger.Error(token.Error())
		}
	}
}

// Publish a message on a topic
func (m *MQTT) Publish(topic string, message string, qos byte) {
	token := m.client.Publish(topic, qos, false, message)
	token.Wait()
}

func (m *MQTT) connect() {
	m.connectToken = m.client.Connect().(*paho.ConnectToken)
	if m.connectToken.Wait() && m.connectToken.Error() != nil {
		if !m.connecting {
			logger.Errorf("MQTT client %s", m.connectToken.Error())
			m.retryConnect()
		}
	}
}

// retryConnect starts a ticker which tries to connect every xx seconds and stops the ticker
// when a connection is established. This is useful when MQTT Broker and GOST are hosted on the same
// machine and GOST is started before mosquito
func (m *MQTT) retryConnect() {
	logger.Infof("MQTT client starting reconnect procedure in background")
	m.connecting = true
	ticker := time.NewTicker(time.Second * 5)
	go func() {
		for range ticker.C {
			m.connect()
			if m.client.IsConnected() {
				ticker.Stop()
				m.connecting = false
			}
		}
	}()
}

func (m *MQTT) connectHandler(c paho.Client) {
	logger.Infof("MQTT client connected")
	hasSession := m.connectToken.SessionPresent()
	logger.Infof("MQTT Session present: %v", hasSession)

	// on first connect, connection lost and persistance is off or no previous session found
	if !m.disconnected || (m.disconnected && !m.persistent) || !hasSession {
		m.subscribe()
	}

	m.disconnected = false
}

//ToDo: bubble up and call retryConnect?
func (m *MQTT) connectionLostHandler(c paho.Client, err error) {
	logger.Warnf("MQTT client lost connection: %v", err)
	m.disconnected = true
	m.retryConnect()
}
