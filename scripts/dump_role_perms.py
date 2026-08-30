#!/usr/bin/env python
# -*- coding: utf-8 -*-
import os, psycopg2, sys
try:
    sys.stdout.reconfigure(encoding='utf-8')
except: pass

c = psycopg2.connect(host='127.0.0.1', port=5432, user='postgres',
    password=os.environ.get('PGPASSWORD',''), dbname='collectai')
cur = c.cursor()

print("=== roles ===")
cur.execute("SELECT id, name, scope, is_builtin FROM roles ORDER BY id")
for r in cur.fetchall():
    print(f"  {r[0]:<10} {r[1]:<12} {r[2]:<10} builtin={r[3]}")

print("\n=== permissions ===")
cur.execute("SELECT id, domain, action FROM permissions ORDER BY domain, action")
for r in cur.fetchall():
    print(f"  {r[0]:<25} {r[1]:<12} {r[2]}")

print("\n=== role_permissions ===")
cur.execute("""SELECT role_id, array_agg(perm_id ORDER BY perm_id) AS perms
               FROM role_permissions GROUP BY role_id ORDER BY role_id""")
for r in cur.fetchall():
    perms = " ".join(r[1])
    print(f"  {r[0]:<10} {perms}")

print("\n=== users ===")
cur.execute("SELECT id, name, role, COALESCE(\"group\",'') AS grp, status FROM users ORDER BY id")
for r in cur.fetchall():
    print(f"  {r[0]:<12} {r[1]:<20} role={r[2]:<8} group={r[3]:<8} status={r[4]}")

print("\n=== user_roles (N:M) ===")
cur.execute("""SELECT u.id, ur.role_id, ur.scope_type, ur.scope_id, ur.is_primary
               FROM user_roles ur JOIN users u ON u.id=ur.user_id
               ORDER BY u.id""")
for r in cur.fetchall():
    print(f"  {r[0]:<12} -> {r[1]:<10} scope={r[2]}/{r[3]:<6} primary={r[4]}")
c.close()
