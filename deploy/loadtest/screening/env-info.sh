#!/usr/bin/env bash
# Records the environment of a benchmark run (hardware, OS, PostgreSQL version and every non-default setting, load, commit).
# Needs DATABASE_URL (owner); prints no credential.
set -euo pipefail
echo "date_utc=$(date -u +%FT%TZ)"
echo "commit=$(git -C "$(dirname "$0")" rev-parse HEAD)"
echo "kernel=$(uname -sr)"
echo "cpus=$(nproc) model=$(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2 | sed 's/^ //')"
echo "mem_total_mb=$(awk '/MemTotal/{print int($2/1024)}' /proc/meminfo)"
echo "loadavg=$(cut -d' ' -f1-3 /proc/loadavg)"
echo "disk=$(df -h / | tail -1 | awk '{print $2" total, "$4" free"}')"
psql "$DATABASE_URL" -Atc "select version()"
echo "-- pg settings whose source is not 'default' (everything else is the compiled-in default):"
psql "$DATABASE_URL" -Atc "select name||'='||current_setting(name)||'  ['||source||']' from pg_settings where source not in ('default','override') order by name"
echo "-- settings relevant to the measurements:"
psql "$DATABASE_URL" -Atc "select name||'='||current_setting(name) from pg_settings where name in ('shared_buffers','work_mem','maintenance_work_mem','effective_cache_size','max_connections','random_page_cost','max_parallel_workers_per_gather','jit','track_io_timing','synchronous_commit','fsync','autovacuum','default_statistics_target') order by name"
echo "-- processes using CPU right now (top 5):"
ps -eo pid,pcpu,comm --sort=-pcpu | head -6
