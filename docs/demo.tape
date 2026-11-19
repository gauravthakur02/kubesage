# Records assets/demo.gif (about 20 seconds) with VHS (https://github.com/charmbracelet/vhs).
# Record against a throwaway kind cluster so no real cluster or namespace names end up in the GIF:
#   export KUBECONFIG=$(mktemp -d)/config      # keep your real kubeconfig out of the recording
#   kind create cluster --name demo
#   kubectl create ns shop
#   kubectl -n shop create deployment web --image=nginx:1.27-alpine --replicas=3
#   kubectl -n shop create deployment api --image=nginx:1.27-alpine --replicas=2
#   kubectl -n shop create deployment checkout --image=nginx:9.9-does-not-exist
#   kubectl -n shop create deployment worker --image=busybox:1.36 -- sh -c \
#     'echo "connecting to db..."; sleep 2; echo "FATAL: DATABASE_URL is not set" >&2; exit 1'
#   kubectl -n shop expose deployment web --port 80
#   DEMO_HOME=$(mktemp -d) && mkdir -p $DEMO_HOME/.config/kubeweave &&
#     printf 'tour_done: true\nupdate_check: false\n' > $DEMO_HOME/.config/kubeweave/config.yaml
#   DEMO_HOME=$DEMO_HOME vhs docs/demo.tape
Output assets/demo.gif
Set Shell bash
Set FontSize 15
Set Width 1280
Set Height 720
Set Padding 16
Set Theme "TokyoNight"
Set TypingSpeed 70ms
Set Framerate 20
Set PlaybackSpeed 1.0

Hide
Type "export HOME=$DEMO_HOME PS1='$ ' && clear" Enter
Sleep 500ms
Type "kubeweave -n shop --readonly" Enter
Sleep 4s
Show

# the pod table, then the crashing pod's logs
Sleep 2.5s
Type "/worker" Sleep 300ms Enter Sleep 600ms
Type "l" Sleep 3s
Escape Sleep 400ms
Escape Sleep 300ms

# Pulse: health bars and detected issues, no AI needed
Type "H" Sleep 4.5s
Escape Sleep 400ms

# relationships of a deployment
Type ":deploy" Sleep 200ms Enter Sleep 800ms
Type "/web" Sleep 200ms Enter Sleep 500ms
Type "w" Sleep 3.5s

Hide
Escape Type "q"
