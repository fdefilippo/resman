#!/bin/bash
# 1. Verifica che i file sorgente contengano le modifiche
grep -n "IO merged for system user" /home/francesco/lavoro/resman/metrics/collector.go
grep -n "State: Exporting IO for system user" /home/francesco/lavoro/resman/state/manager.go

# Forza una ricompilazione pulita
cd /home/francesco/lavoro/resman
rm -f resman
make build
sudo systemctl stop resman
sudo cp resman /usr/bin/resman
sudo systemctl start resman
sleep 45
grep -E "IO merged|UserMetrics created|Exporting IO" /var/log/resman.log | tail -10
