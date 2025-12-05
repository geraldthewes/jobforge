# API Endpoints Specification

## Overview

The Nomad Build Service exposes a RESTful API with WebSocket support for real-time log streaming. All endpoints follow consistent naming and response patterns.

## Endpoints

### submitJob
**Method**: POST
**Path**: `/jobs`
**Description**: Submits a build job with configuration

**Request Body**:
```json
{
  "owner": "string",
  "repo_url": "string",
  "git_ref": "string",
  "git_credentials_path": "string",
  "dockerfile_path": "string",
  "image_tags": ["string"],
  "registry_url": "string",
  "registry_credentials_path": "string",
  "test_commands": ["string"],
  "test_entry_point": boolean
}
```

**Response**:
```json
{
  "job_id": "string"
}
```

### getStatus
**Method**: GET
**Path**: `/jobs/{job_id}/status`
**Description**: Retrieves current status of a job

**Response**:
```json
{
  "status": "string",
  "metrics": {
    "build_duration_seconds": "number",
    "test_duration_seconds": "number",
    "publish_duration_seconds": "number",
    "concurrent_jobs_total": "number"
  },
  "phase_timestamps": {
    "job_start": "string",
    "build_start": "string",
    "build_end": "string",
    "test_start": "string",
    "test_end": "string",
    "publish_start": "string",
    "publish_end": "string",
    "job_end": "string"
  }
}
```

### getLogs
**Method**: GET
**Path**: `/jobs/{job_id}/logs`
**Description**: Retrieves logs for all phases of a job

**Response**:
```json
{
  "build": "string",
  "test": "string",
  "publish": "string"
}
```

### streamLogs
**Method**: GET (WebSocket)
**Path**: `/jobs/{job_id}/logs/stream`
**Description**: Real-time log streaming during builds

### killJob
**Method**: POST
**Path**: `/jobs/{job_id}/kill`
**Description**: Terminates a running job gracefully

**Response**:
```json
{
  "success": boolean
}
```

### cleanup
**Method**: POST
**Path**: `/jobs/{job_id}/cleanup`
**Description**: Removes zombie jobs and temporary artifacts

**Response**:
```json
{
  "success": boolean
}
```

### health
**Method**: GET
**Path**: `/health`
**Description**: Service health check endpoint

**Response**:
```json
{
  "status": "string",
  "timestamp": "string"
}
```

### ready
**Method**: GET
**Path**: `/ready`
**Description**: Readiness probe endpoint

**Response**:
```json
{
  "status": "string",
  "timestamp": "string"
}
```

## Error Handling

All endpoints return appropriate HTTP status codes:
- 200 OK for successful operations
- 400 Bad Request for invalid inputs
- 404 Not Found for unknown jobs
- 500 Internal Server Error for service failures

## Response Format

All JSON responses follow a consistent structure:
```json
{
  "data": {},
  "error": null,
  "timestamp": "string"
}
```

## Authentication

Authentication is handled at the infrastructure level by Nomad ACLs and secure API communication, not within the application itself.