# Records docs/demo.gif with VHS (https://github.com/charmbracelet/vhs).
# Record against a throwaway kind cluster so no real cluster or namespace names end up in the GIF:
#   kind create cluster --name demo
#   kubectl create deployment web --image=nginx:1.27-alpine --replicas=3
#   kubectl create deployment broken --image=nginx:does-not-exist
#   vhs docs/demo.tape
Output docs/demo.gif
Set Shell bash
Set FontSize 14
Set Width 1400
Set Height 800
Set Theme "TokyoNight"
Set TypingSpeed 60ms

Hide
Type "kubeweave --context kind-demo -n default --readonly" Enter
Sleep 3s
Show
Sleep 2s
Down Sleep 500ms Down Sleep 500ms
Type "w" Sleep 3s
Escape Sleep 1s
Type "t" Sleep 3s
Escape Sleep 1s
Type "2" Sleep 2s
Type " " Sleep 2s
Escape Sleep 1s
Ctrl+P Sleep 1s
Type "fleet" Sleep 1s Enter Sleep 4s
Escape Sleep 1s
Type ":history" Enter Sleep 3s
Escape Sleep 1s
Type "H" Sleep 4s
Type "q"
