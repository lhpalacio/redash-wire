# Records the README demo GIF. Needs the dev stack up (make dev-setup),
# a built binary (make build), and psql. Render with: vhs dev/demo.tape
Output dev/demo.gif

Require psql

Set Shell "bash"
Set Theme "Catppuccin Mocha"
Set FontSize 14
Set Width 1200
Set Height 720
Set Padding 20
Set Framerate 20
Set TypingSpeed 40ms

# Run from a clean HOME so the proxy resolves the config like a real install
# (~/.redash-wire/config.yaml) instead of warning about the cwd config.
Hide
Type "export PATH=$PWD/bin:$PATH; export HOME=$(mktemp -d /tmp/demo.XXXXXX); mkdir -p $HOME/.redash-wire; cp config.yaml $HOME/.redash-wire/; cd $HOME; clear"
Enter
Show

Type "redash-wire &"
Enter
Sleep 4.5s
Enter
Sleep 1s

Type "PGPASSWORD=supersecret psql -h 127.0.0.1 -p 15432 -U redash-wire -d 'Sample PostgreSQL'"
Enter
Sleep 2.5s

Type@30ms "SELECT status, count(*), sum(amount) FROM orders GROUP BY status;"
Enter
Sleep 4s

Type "\q"
Enter
Sleep 2s
