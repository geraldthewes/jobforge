#!/usr/bin/env python3
"""
Docker Registry Cleanup Script

This script helps clean up old temporary images from a Docker registry.
It supports both individual repository cleanup and bulk cleanup with pattern matching.

Features:
- List all repositories in registry
- Clean up specific repositories or patterns (e.g., bdtemp*)
- Proper manifest deletion using Docker Registry v2 API
- Support for authentication
- Dry-run mode for safety
- Comprehensive logging
- Error handling and retry logic

Usage:
    python3 cleanup_registry.py --registry https://registry.cluster:5000
    python3 cleanup_registry.py --registry https://registry.cluster:5000 --pattern "bdtemp*"
    python3 cleanup_registry.py --registry https://registry.cluster:5000 --repository bdtemp
    python3 cleanup_registry.py --registry https://registry.cluster:5000 --dry-run

Requirements:
    pip install requests
"""

import argparse
import json
import re
import sys
from typing import List, Optional, Dict, Tuple
import requests
import urllib3
from urllib.parse import urlparse

# Disable SSL warnings for self-signed certificates
urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)


class RegistryCleanup:
    def __init__(self, registry_url: str, username: Optional[str] = None,
                 password: Optional[str] = None, verify_ssl: bool = False, dry_run: bool = False):
        """Initialize the registry cleanup client."""
        self.registry_url = registry_url.rstrip('/')
        self.verify_ssl = verify_ssl
        self.dry_run = dry_run
        self.session = requests.Session()

        # Setup authentication if provided
        if username and password:
            self.session.auth = (username, password)
            print(f"✓ Using authentication for user: {username}")

        # Setup SSL verification
        if not verify_ssl:
            self.session.verify = False
            print("⚠ SSL verification disabled")

        print(f"📡 Registry URL: {self.registry_url}")
        print(f"🔍 Dry run mode: {'ENABLED' if dry_run else 'DISABLED'}")

    def _make_request(self, method: str, path: str, **kwargs) -> requests.Response:
        """Make HTTP request to registry with error handling."""
        url = f"{self.registry_url}/v2{path}"
        try:
            response = self.session.request(method, url, **kwargs)
            response.raise_for_status()
            return response
        except requests.exceptions.RequestException as e:
            print(f"❌ Request failed: {method} {url} - {e}")
            raise

    def list_repositories(self) -> List[str]:
        """List all repositories in the registry with pagination support."""
        print("\n📋 Listing all repositories...")
        all_repositories = []
        last_repo = None

        try:
            while True:
                # Build URL with pagination
                if last_repo:
                    path = f'/_catalog?last={last_repo}&n=100'
                else:
                    path = '/_catalog?n=100'

                response = self._make_request('GET', path)
                data = response.json()
                repositories = data.get('repositories', [])

                if not repositories:
                    break

                all_repositories.extend(repositories)

                # Check if there are more pages
                # If we got fewer than requested, we're done
                if len(repositories) < 100:
                    break

                last_repo = repositories[-1]

            print(f"✓ Found {len(all_repositories)} repositories")
            return all_repositories
        except Exception as e:
            print(f"❌ Failed to list repositories: {e}")
            # Return what we got so far
            if all_repositories:
                print(f"⚠ Returning {len(all_repositories)} repositories fetched before error")
                return all_repositories
            return []

    def list_tags(self, repository: str) -> List[str]:
        """List all tags for a specific repository."""
        try:
            response = self._make_request('GET', f'/{repository}/tags/list')
            data = response.json()
            tags = data.get('tags', [])
            if tags:
                print(f"  📦 Repository '{repository}' has {len(tags)} tags")
                return tags
            else:
                print(f"  📦 Repository '{repository}' has no tags")
                return []
        except requests.exceptions.HTTPError as e:
            if e.response.status_code == 404:
                print(f"  📦 Repository '{repository}' not found")
                return []
            raise
        except Exception as e:
            print(f"❌ Failed to list tags for '{repository}': {e}")
            return []

    def get_manifest_digest(self, repository: str, tag: str) -> Optional[str]:
        """Get the digest for a specific manifest."""
        # Try different manifest types
        manifest_types = [
            'application/vnd.docker.distribution.manifest.v2+json',
            'application/vnd.docker.distribution.manifest.v1+json',
            'application/vnd.oci.image.manifest.v1+json'
        ]

        for manifest_type in manifest_types:
            try:
                response = self._make_request(
                    'HEAD',
                    f'/{repository}/manifests/{tag}',
                    headers={'Accept': manifest_type}
                )
                digest = response.headers.get('Docker-Content-Digest')
                if digest:
                    return digest
            except requests.exceptions.HTTPError as e:
                if e.response.status_code == 404:
                    continue  # Try next manifest type
                raise
            except Exception as e:
                continue  # Try next manifest type

        print(f"    ⚠ No digest found for {repository}:{tag} (tried all manifest types)")
        return None

    def delete_manifest(self, repository: str, digest: str) -> bool:
        """Delete a manifest by its digest."""
        if self.dry_run:
            print(f"    🔍 DRY RUN: Would delete manifest {digest}")
            return True

        try:
            response = self._make_request('DELETE', f'/{repository}/manifests/{digest}')
            if response.status_code in [202, 204]:
                print(f"    ✓ Deleted manifest {digest}")
                return True
            else:
                print(f"    ⚠ Unexpected response {response.status_code} for {digest}")
                return False
        except Exception as e:
            print(f"    ❌ Failed to delete manifest {digest}: {e}")
            return False

    def cleanup_repository(self, repository: str) -> Tuple[int, int]:
        """Clean up all tags in a specific repository."""
        print(f"\n🧹 Cleaning up repository: {repository}")

        tags = self.list_tags(repository)
        if not tags:
            print(f"  ℹ️  Repository is empty (no tags found)")
            return 0, 0

        deleted_count = 0
        failed_count = 0

        for tag in tags:
            print(f"  🏷 Processing tag: {tag}")

            # Get manifest digest
            digest = self.get_manifest_digest(repository, tag)
            if not digest:
                failed_count += 1
                continue

            # Delete manifest
            if self.delete_manifest(repository, digest):
                deleted_count += 1
            else:
                failed_count += 1

        print(f"  📊 Repository '{repository}': {deleted_count} deleted, {failed_count} failed")
        return deleted_count, failed_count

    def cleanup_repositories_by_pattern(self, pattern: str) -> Dict[str, Tuple[int, int]]:
        """Clean up repositories matching a pattern (supports wildcards)."""
        print(f"\n🔍 Finding repositories matching pattern: {pattern}")

        repositories = self.list_repositories()
        if not repositories:
            print("❌ No repositories found")
            return {}

        # Convert shell-style pattern to regex
        regex_pattern = pattern.replace('*', '.*').replace('?', '.')
        regex = re.compile(f'^{regex_pattern}$')

        matching_repos = [repo for repo in repositories if regex.match(repo)]

        if not matching_repos:
            print(f"❌ No repositories match pattern: {pattern}")
            return {}

        print(f"✓ Found {len(matching_repos)} repositories matching pattern:")
        for repo in matching_repos:
            print(f"  - {repo}")

        # Confirm deletion if not in dry-run mode
        if not self.dry_run:
            print(f"\n⚠ This will delete ALL images in {len(matching_repos)} repositories!")
            confirm = input("Type 'yes' to confirm deletion: ")
            if confirm.lower() != 'yes':
                print("❌ Operation cancelled")
                return {}

        results = {}
        total_deleted = 0
        total_failed = 0
        empty_repos = 0

        for repo in matching_repos:
            deleted, failed = self.cleanup_repository(repo)
            results[repo] = (deleted, failed)
            total_deleted += deleted
            total_failed += failed
            if deleted == 0 and failed == 0:
                empty_repos += 1

        print(f"\n📊 TOTAL SUMMARY:")
        print(f"  Repositories processed: {len(matching_repos)}")
        print(f"  Empty repositories (no tags): {empty_repos}")
        print(f"  Images deleted: {total_deleted}")
        print(f"  Failed deletions: {total_failed}")

        if empty_repos > 0:
            print(f"\n💡 Found {empty_repos} empty repository entries in the catalog.")
            print(f"   These are leftover catalog entries after manifests were deleted.")
            print(f"   Run registry garbage collection to remove them:")
            print(f"   docker exec <registry-container> registry garbage-collect /etc/docker/registry/config.yml")

        return results

    def show_registry_info(self):
        """Display information about the registry."""
        print(f"\n📡 Registry Information")
        print(f"  URL: {self.registry_url}")

        try:
            # Test connectivity
            response = self._make_request('GET', '/')
            print(f"  ✓ Registry is accessible")
        except Exception as e:
            print(f"  ❌ Registry is not accessible: {e}")
            return

        # List repositories
        repositories = self.list_repositories()
        if repositories:
            print(f"\n📋 Repositories ({len(repositories)}):")
            for repo in repositories[:10]:  # Show first 10
                tags = self.list_tags(repo)
                print(f"  - {repo} ({len(tags)} tags)")

            if len(repositories) > 10:
                print(f"  ... and {len(repositories) - 10} more repositories")


def main():
    parser = argparse.ArgumentParser(
        description='Clean up Docker registry repositories and tags',
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  # Show registry information
  python3 cleanup_registry.py --registry https://registry.cluster:5000 --info

  # Clean up all repositories starting with "bdtemp"
  python3 cleanup_registry.py --registry https://registry.cluster:5000 --pattern "bdtemp*"

  # Clean up specific repository
  python3 cleanup_registry.py --registry https://registry.cluster:5000 --repository bdtemp

  # Dry run (preview what would be deleted)
  python3 cleanup_registry.py --registry https://registry.cluster:5000 --pattern "bdtemp*" --dry-run

  # With authentication
  python3 cleanup_registry.py --registry https://registry.cluster:5000 --username admin --password secret --pattern "bdtemp*"
        """
    )

    parser.add_argument(
        '--registry',
        required=True,
        help='Registry URL (e.g., https://registry.cluster:5000)'
    )
    parser.add_argument(
        '--pattern',
        help='Repository pattern to match (supports wildcards like bdtemp*)'
    )
    parser.add_argument(
        '--repository',
        help='Specific repository to clean up'
    )
    parser.add_argument(
        '--username',
        help='Registry username for authentication'
    )
    parser.add_argument(
        '--password',
        help='Registry password for authentication'
    )
    parser.add_argument(
        '--dry-run',
        action='store_true',
        help='Preview what would be deleted without actually deleting'
    )
    parser.add_argument(
        '--info',
        action='store_true',
        help='Show registry information and exit'
    )
    parser.add_argument(
        '--verify-ssl',
        action='store_true',
        help='Verify SSL certificates (default: false for self-signed certs)'
    )

    args = parser.parse_args()

    # Validate arguments
    if not args.info and not args.pattern and not args.repository:
        print("❌ Error: Must specify --pattern, --repository, or --info")
        parser.print_help()
        sys.exit(1)

    if args.pattern and args.repository:
        print("❌ Error: Cannot specify both --pattern and --repository")
        sys.exit(1)

    # Initialize cleanup client
    try:
        cleanup = RegistryCleanup(
            registry_url=args.registry,
            username=args.username,
            password=args.password,
            verify_ssl=args.verify_ssl,
            dry_run=args.dry_run
        )
    except Exception as e:
        print(f"❌ Failed to initialize registry client: {e}")
        sys.exit(1)

    # Execute requested operation
    try:
        if args.info:
            cleanup.show_registry_info()
        elif args.pattern:
            cleanup.cleanup_repositories_by_pattern(args.pattern)
        elif args.repository:
            deleted, failed = cleanup.cleanup_repository(args.repository)
            print(f"\n📊 SUMMARY: {deleted} deleted, {failed} failed")

        # Always show garbage collection instructions (for both dry-run and actual cleanup)
        if args.pattern or args.repository:
            print(f"\n" + "="*70)
            print(f"📋 NEXT STEP: Run Registry Garbage Collection")
            print(f"="*70)
            print(f"\nTo remove empty catalog entries and free disk space, run garbage")
            print(f"collection on your registry host:")
            print(f"\n  # If registry is running in Docker:")
            print(f"  docker exec <registry-container> registry garbage-collect /etc/docker/registry/config.yml")
            print(f"\n  # If registry is running in Kubernetes/Nomad:")
            print(f"  kubectl exec <registry-pod> -- registry garbage-collect /etc/docker/registry/config.yml")
            print(f"  nomad alloc exec <registry-alloc> registry garbage-collect /etc/docker/registry/config.yml")
            print(f"\n  # If registry is a system service:")
            print(f"  ssh <registry-host> 'registry garbage-collect /etc/docker/registry/config.yml'")
            print(f"\n" + "="*70)

    except KeyboardInterrupt:
        print(f"\n⚠ Operation interrupted by user")
        sys.exit(1)
    except Exception as e:
        print(f"❌ Operation failed: {e}")
        sys.exit(1)


if __name__ == '__main__':
    main()