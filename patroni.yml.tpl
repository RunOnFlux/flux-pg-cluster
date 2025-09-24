scope: postgres-cluster
namespace: /patroni/
name: __MY_NAME__

log:
  level: DEBUG
  format: '%(asctime)s %(levelname)s: %(message)s'

restapi:
  listen: 0.0.0.0:__PATRONI_API_PORT__
  connect_address: __MY_IP__:__HOST_PATRONI_API_PORT__
  allowlist_include_members: true
  certfile: /etc/ssl/cluster/patroni/server.crt
  keyfile: /etc/ssl/cluster/patroni/server.key
  cafile: /etc/ssl/cluster/ca/ca.crt

etcd:
  hosts: __ETCD_HOSTS__
  protocol: __ETCD_PROTOCOL__
  cacert: /etc/ssl/cluster/ca/ca.crt
  cert: /etc/ssl/cluster/etcd/client.crt
  key: /etc/ssl/cluster/etcd/client.key

bootstrap:
  method: initdb
  dcs:
    ttl: 30
    loop_wait: 10
    retry_timeout: 30
    maximum_lag_on_failover: 1048576
    master_start_timeout: 300
    synchronous_mode: false
    postgresql:
      use_pg_rewind: true
      use_slots: true
      parameters:
        max_connections: 200
        shared_buffers: 256MB
        effective_cache_size: 1GB
        maintenance_work_mem: 64MB
        checkpoint_completion_target: 0.9
        wal_buffers: 16MB
        default_statistics_target: 100
        random_page_cost: 1.1
        effective_io_concurrency: 200
        work_mem: 4MB
        min_wal_size: 1GB
        max_wal_size: 4GB
        hot_standby: on
        wal_level: replica
        max_wal_senders: 10
        max_replication_slots: 10
        wal_log_hints: on

  initdb:
  - encoding: UTF8
  - data-checksums
  - auth-host: md5
  - auth-local: peer

  pg_hba:
  - hostssl replication replicator 0.0.0.0/0 cert clientcert=verify-full
  - hostssl all all 0.0.0.0/0 md5
  - host replication replicator 0.0.0.0/0 md5
  - host all all 0.0.0.0/0 md5

  users:
    admin:
      password: __POSTGRES_SUPERUSER_PASSWORD__
      options:
        - createrole
        - createdb

postgresql:
  listen: 0.0.0.0:__POSTGRES_PORT__
  connect_address: __MY_IP__:__HOST_POSTGRES_PORT__
  data_dir: /var/lib/postgresql/data
  bin_dir: /usr/lib/postgresql/14/bin
  pgpass: /tmp/pgpass
  authentication:
    replication:
      username: replicator
      password: __POSTGRES_REPLICATION_PASSWORD__
    superuser:
      username: postgres
      password: __POSTGRES_SUPERUSER_PASSWORD__
  parameters:
    unix_socket_directories: '/var/run/postgresql'
    ssl: '__SSL_ENABLED__'
    ssl_cert_file: '/etc/ssl/cluster/postgres/server.crt'
    ssl_key_file: '/etc/ssl/cluster/postgres/server.key'
    ssl_ca_file: '/etc/ssl/cluster/ca/ca.crt'
    ssl_crl_file: ''
    ssl_prefer_server_ciphers: on
    ssl_ciphers: 'ECDHE-ECDSA-AES256-GCM-SHA384:ECDHE-RSA-AES256-GCM-SHA384:ECDHE-ECDSA-CHACHA20-POLY1305:ECDHE-RSA-CHACHA20-POLY1305:ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-RSA-AES128-GCM-SHA256'

tags:
  nofailover: false
  noloadbalance: false
  clonefrom: false
  nosync: false