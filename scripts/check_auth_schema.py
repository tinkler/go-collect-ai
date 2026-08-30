#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""Check actual auth table schemas in PG"""
import os, sys
try:
    import psycopg2
except ImportError:
    print("install: pip install psycopg2-binary")
    sys.exit(1)

conn = psycopg2.connect(
    host='127.0.0.1', port=5432,
    user='postgres',
    password=os.environ.get('PGPASSWORD', ''),
    dbname='collectai',
)
cur = conn.cursor()
for t in ['users', 'role_permissions', 'auth_sessions', 'audit_log']:
    print(f"\n=== {t} ===")
    cur.execute("""
        SELECT column_name, data_type
        FROM information_schema.columns
        WHERE table_name=%s
        ORDER BY ordinal_position
    """, (t,))
    rows = cur.fetchall()
    if not rows:
        print("  (table does not exist)")
    for r in rows:
        print(f"  {r[0]:25s} {r[1]}")

print("\n=== role_permissions sample ===")
try:
    cur.execute("SELECT * FROM role_permissions LIMIT 5")
    for r in cur.fetchall():
        print(" ", r)
except Exception as e:
    print(" err:", e)

print("\n=== users sample ===")
try:
    cur.execute("SELECT id, name, role, tenant_id, source, status FROM users LIMIT 5")
    for r in cur.fetchall():
        print(" ", r)
except Exception as e:
    print(" err:", e)

conn.close()
