# Records the first-run setup wizard GIF for the README. Needs the dev stack up
# (make dev-setup) and a built binary (make build). The API key is substituted at
# record time so it never lands in the repo:
#   sed "s/__REDASH_API_KEY__/<your dev api key>/" dev/wizard.tape > /tmp/wizard.tape
#   vhs /tmp/wizard.tape
Output dev/wizard.gif

Set Shell "bash"
Set Theme "Catppuccin Mocha"
Set FontSize 14
Set Width 1200
Set Height 720
Set Padding 20
Set Framerate 20
Set TypingSpeed 40ms

# Empty HOME and cwd with no config so the first-run wizard triggers.
Hide
Type "export PATH=$PWD/bin:$PATH; export HOME=$(mktemp -d /tmp/demo.XXXXXX); cd $HOME; clear"
Enter
Show

Type "redash-wire"
Enter
Sleep 1.5s

Type "default"
Sleep 500ms
Enter
Sleep 500ms

Type "http://localhost:5050"
Sleep 500ms
Enter
Sleep 500ms

# Submitting the API key completes the form; the wizard then tests the
# connection automatically, writes the config, and starts the server.
Type "__REDASH_API_KEY__"
Sleep 500ms
Enter
Sleep 3s

Sleep 4s
