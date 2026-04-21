package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	MessagesReceived = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "subscription_handler_messages_received_total",
		Help: "Total number of Telegram messages received.",
	})

	SubscriptionsCreated = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "subscription_handler_subscriptions_created_total",
		Help: "Total number of subscriptions successfully created.",
	})

	CommandsReceived = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "subscription_handler_commands_total",
		Help: "Total number of bot commands received, by command name.",
	}, []string{"command"})
)

func init() {
	prometheus.MustRegister(MessagesReceived, SubscriptionsCreated, CommandsReceived)
}