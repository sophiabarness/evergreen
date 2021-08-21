package route

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/evergreen-ci/evergreen"
	"github.com/evergreen-ci/evergreen/model/host"
	"github.com/evergreen-ci/evergreen/rest/data"
	"github.com/mongodb/amboy/queue"
	"github.com/mongodb/grip"
	"github.com/mongodb/grip/level"
	"github.com/mongodb/grip/message"
	"github.com/mongodb/grip/send"
	sns "github.com/robbiet480/go.sns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEC2SNSRoute(t *testing.T) {
	rh := ec2SNS{baseSNS{messageType: "unknown"}}
	responder := rh.Run(context.Background())
	assert.Equal(t, http.StatusBadRequest, responder.Status())

	defer func(s send.Sender) {
		assert.NoError(t, grip.SetSender(s))
	}(grip.GetSender())

	sender := send.MakeInternalLogger()
	assert.NoError(t, grip.SetSender(sender))
	rh.messageType = messageTypeSubscriptionConfirmation
	responder = rh.Run(context.Background())
	assert.Equal(t, http.StatusOK, responder.Status())
	assert.True(t, sender.HasMessage())
	m := sender.GetMessage()
	assert.Equal(t, level.Alert, m.Priority)
	assert.Equal(t, "got AWS SNS subscription confirmation. Visit subscribe_url to confirm", m.Message.Raw().(message.Fields)["message"])
}

func TestHandleEC2SNSNotification(t *testing.T) {
	ctx := context.Background()

	rh := ec2SNS{}
	rh.queue = queue.NewLocalLimitedSize(1, 1)
	require.NoError(t, rh.queue.Start(ctx))
	rh.payload = sns.Payload{Message: `{"version":"0","id":"qwertyuiop","detail-type":"EC2 Instance State-change Notification","source":"sns.ec2","time":"2020-07-23T14:48:37Z","region":"us-east-1","resources":["arn:aws:ec2:us-east-1:1234567890:instance/i-0123456789"],"detail":{"instance-id":"i-0123456789","state":"terminated"}}`}

	// unknown host
	rh.sc = &data.MockConnector{}
	assert.NoError(t, rh.handleNotification(ctx))
	assert.Equal(t, rh.queue.Stats(ctx).Total, 0)

	// known host
	rh.sc = &data.MockConnector{MockHostConnector: data.MockHostConnector{CachedHosts: []host.Host{{Id: "i-0123456789"}}}}
	assert.NoError(t, rh.handleNotification(ctx))
	require.Equal(t, 1, rh.queue.Stats(ctx).Total)
	assert.True(t, strings.HasPrefix(rh.queue.Next(ctx).ID(), "host-monitoring-external-state-check"))
}

func TestEC2SNSNotificationHandlers(t *testing.T) {
	ctx := context.Background()
	agentHost := host.Host{
		Id:        "agent_host",
		StartTime: time.Date(2009, time.November, 10, 23, 0, 0, 0, time.UTC),
		StartedBy: evergreen.User,
		Provider:  evergreen.ProviderNameMock,
	}
	spawnHost := host.Host{
		Id:        "spawn_host",
		StartedBy: "user",
		UserHost:  true,
	}
	messageID := "m0"
	rh := ec2SNS{}
	rh.payload.MessageId = messageID
	rh.sc = &data.MockConnector{MockHostConnector: data.MockHostConnector{
		CachedHosts: []host.Host{
			agentHost,
			spawnHost,
		},
	},
	}

	for name, test := range map[string]func(*testing.T){
		"InstanceInterruptionWarningInitiatesTermination": func(t *testing.T) {
			rh.payload = sns.Payload{MessageId: messageID}
			require.NoError(t, rh.handleInstanceInterruptionWarning(ctx, agentHost.Id))
			require.Equal(t, 1, rh.queue.Stats(ctx).Total)
		},
		"InstanceTerminatedInitiatesInstanceStatusCheck": func(t *testing.T) {
			require.NoError(t, rh.handleInstanceTerminated(ctx, agentHost.Id))
			require.Equal(t, 1, rh.queue.Stats(ctx).Total)
		},
		"InstanceStoppedWithAgentHostInitiatesInstanceStatusCheck": func(t *testing.T) {
			require.NoError(t, rh.handleInstanceStopped(ctx, agentHost.Id))
			require.Equal(t, 1, rh.queue.Stats(ctx).Total)
		},
		"InstanceStoppedWithSpawnHostNoops": func(t *testing.T) {
			require.NoError(t, rh.handleInstanceStopped(ctx, spawnHost.Id))
			assert.Zero(t, rh.queue.Stats(ctx).Total)
		},
	} {
		rh.queue = queue.NewLocalLimitedSize(1, 1)
		require.NoError(t, rh.queue.Start(ctx))

		t.Run(name, test)
	}
}