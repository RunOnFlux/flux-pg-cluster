# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.3] - 2025-09-15

### Added
- Added diagnostic script (`/app/diagnose.sh`) for comprehensive cluster troubleshooting
- Added explicit bootstrap method configuration in Patroni
- Added etcd health check before Patroni startup
- Enhanced Patroni startup sequence with proper etcd dependency wait

### Fixed
- Fixed Patroni bootstrap issues by explicitly defining initdb method
- Improved Patroni startup reliability by waiting for etcd to be ready
- Added proper initdb authentication configuration
- Increased Patroni startup delay to allow etcd to fully initialize

### Changed
- Enhanced logging for Patroni startup sequence
- Added etcd readiness check before starting Patroni

## [1.0.2] - 2025-09-15

### Changed
- Increased API update frequency from 60 seconds to 5 minutes (300 seconds) to reduce API load
- Added documentation about Patroni's role in PostgreSQL management
- Improved startup messaging with clear instructions about PostgreSQL startup expectations

### Fixed
- Clarified that PostgreSQL should be managed by Patroni, not started manually
- Added diagnostic notes for troubleshooting PostgreSQL startup issues

## [1.0.1] - 2025-09-15

### Fixed
- Removed manual PostgreSQL initialization conflicts with Patroni
- Fixed etcd port binding issues (removed duplicate localhost binding)
- Resolved PostgreSQL authentication failures by letting Patroni handle bootstrap
- Fixed etcd data directory permissions (changed to 700 as required)
- Removed conflicting ETCD_UNSUPPORTED_ARCH environment variable

### Added
- Version tracking system with VERSION file
- Version display in startup logs
- CHANGELOG.md for tracking project changes
- Version badges in README.md

### Changed
- Simplified entrypoint.sh to let Patroni handle PostgreSQL initialization
- Updated etcd configuration to bind only to 0.0.0.0 interface
- Enhanced logging and troubleshooting capabilities

## [1.0.0] - 2025-09-15

### Added
- Initial release of Dynamic PostgreSQL Cluster with Patroni and Flux Integration
- Docker-based deployment with single container containing PostgreSQL 14, etcd, Patroni
- Dynamic cluster member discovery via Flux API
- Automatic cluster membership management (add/remove nodes)
- Comprehensive logging and debugging support
- Port mapping support for external access
- Version tracking system
- Supervisord process management
- Complete documentation and README

### Features
- Self-configuring PostgreSQL cluster
- High availability with Patroni
- etcd for distributed coordination
- API-driven node discovery and management
- Automatic failover and replication
- Health monitoring and status reporting
- Flexible port configuration
- Debug logging and troubleshooting tools

### Technical Details
- PostgreSQL 14 with streaming replication
- Patroni for cluster management
- etcd 3.3.25 for consensus
- Ubuntu 22.04 base image
- Python 3 runtime
- Comprehensive bash scripting for automation