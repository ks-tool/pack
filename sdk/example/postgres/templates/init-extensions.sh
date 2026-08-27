#!/bin/bash
# extensions every database of the stand gets; idempotent
set -euo pipefail
psql -v ON_ERROR_STOP=1 -c 'CREATE EXTENSION IF NOT EXISTS pg_stat_statements'
