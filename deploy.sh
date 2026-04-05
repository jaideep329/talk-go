#!/bin/bash
set -e

SERVER="ubuntu@3.110.79.66"
KEY="~/CLProd.pem"
PROJECT_DIR="~/talk-go"

ssh -i $KEY $SERVER << 'EOF'
export PATH=$PATH:/usr/local/go/bin
cd ~/talk-go
git pull
go build -o talk-go .
pkill talk-go || true
nohup ./talk-go > app.log 2>&1 &
echo "Deployed. PID: $!"
EOF
