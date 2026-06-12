#!/usr/bin/env bash
set -euo pipefail

REDASH_URL="http://localhost:5050"

echo "==> Waiting for Redash to be ready..."
until curl -sf "$REDASH_URL/ping" > /dev/null 2>&1; do
  sleep 2
done
echo "==> Redash is up!"

echo "==> Creating Redash database tables..."
docker compose exec -T redash-server python /app/manage.py database create_tables

echo "==> Creating admin user..."
API_KEY=$(docker compose exec -T redash-server python -c "
from redash.models import db, User, Organization, Group
from redash import create_app

app = create_app()
with app.app_context():
    # Check if admin already exists
    existing = User.query.filter_by(email='admin@example.com').first()
    if existing:
        print(existing.api_key)
    else:
        org = Organization.query.first()
        if not org:
            org = Organization(name='Default', slug='default', settings={})
            db.session.add(org)
            db.session.flush()

            admin_group = Group(name='admin', permissions=['admin','super_admin'], org=org, type=Group.BUILTIN_GROUP)
            default_group = Group(name='default', permissions=Group.DEFAULT_PERMISSIONS, org=org, type=Group.BUILTIN_GROUP)
            db.session.add_all([admin_group, default_group])
            db.session.flush()

        admin_group = Group.query.filter_by(name='admin', org=org).first()
        default_group = Group.query.filter_by(name='default', org=org).first()

        user = User(org=org, name='Admin', email='admin@example.com', group_ids=[admin_group.id, default_group.id])
        user.hash_password('admin')
        db.session.add(user)
        db.session.commit()
        print(user.api_key)
" 2>/dev/null)

if [ -z "$API_KEY" ]; then
  echo "ERROR: Failed to get API key."
  exit 1
fi

echo "==> Admin API key: $API_KEY"

echo "==> Adding sample PostgreSQL data source..."
DS_RESPONSE=$(curl -sf "$REDASH_URL/api/data_sources" \
  -H "Authorization: Key $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Sample PostgreSQL",
    "type": "pg",
    "options": {
      "host": "sample-db",
      "port": 5432,
      "dbname": "sample",
      "user": "sample",
      "password": "sample"
    }
  }' 2>&1) || true

echo "$DS_RESPONSE" | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'    Data source ID: {d.get(\"id\", \"already exists\")}')" 2>/dev/null || echo "    (data source may already exist)"

CONFIG_FILE="config.yaml"
if [ -f "$CONFIG_FILE" ]; then
  cp "$CONFIG_FILE" "$CONFIG_FILE.bak"
  echo "==> Backed up existing $CONFIG_FILE to $CONFIG_FILE.bak"
fi
cat > "$CONFIG_FILE" <<EOF
postgres_listen_addr: "127.0.0.1:15432"
mysql_listen_addr: "127.0.0.1:13306"
default_profile: local

profiles:
  local:
    redash_url: "$REDASH_URL"
    api_key: "$API_KEY"
EOF
chmod 600 "$CONFIG_FILE"

echo ""
echo "==> Setup complete!"
echo ""
echo "  Redash UI:  $REDASH_URL  (admin@example.com / admin)"
echo "  API key:    $API_KEY"
echo "  Config:     $CONFIG_FILE (updated)"
echo ""
echo "  Run redash-wire:"
echo "    make run"
echo ""
echo "  Then connect with psql:"
echo "    psql -h 127.0.0.1 -p 15432 -U redash-wire"
