#!/usr/bin/env python3
"""Read-only prerequisites for stack.yaml. Never retrieves secret values or changes AWS."""
import argparse
import ipaddress
import json
import re
import sys
from urllib.parse import urlsplit

REQUIRED = {"VpcId", "PrivateSubnetA", "PrivateSubnetB", "ImageUri", "CertificateArn",
            "PublicUrl", "ClientCidr", "SealingKeySecretArn", "AlarmTopicArn"}
OPTIONAL = {"SealingKeyKmsArn", "DesiredCount", "DatabaseClass", "DatabaseEngineVersion"}


def parameters(rows):
    if not isinstance(rows, list):
        raise ValueError("parameters must be a CloudFormation parameter array")
    result = {}
    for row in rows:
        if not isinstance(row, dict) or set(row) != {"ParameterKey", "ParameterValue"}:
            raise ValueError("each parameter needs ParameterKey and ParameterValue")
        key, value = row["ParameterKey"], row["ParameterValue"]
        if not isinstance(key, str) or key not in REQUIRED | OPTIONAL or key in result:
            raise ValueError("unknown or duplicate parameter")
        if not isinstance(value, str):
            raise ValueError("parameter values must be strings")
        result[key] = value
    missing = sorted(k for k in REQUIRED if not result.get(k))
    if missing:
        raise ValueError("missing parameters: " + ", ".join(missing))
    return result


def dns_matches(pattern, hostname):
    pattern, hostname = pattern.lower().rstrip('.'), hostname.lower().rstrip('.')
    if pattern.startswith('*.'):
        return hostname.count('.') == pattern.count('.') and hostname.endswith(pattern[1:])
    return hostname == pattern


def check(params, region, client):
    blockers, warnings, passed = [], [], []

    def require(ok, message):
        (passed if ok else blockers).append(message)
        return bool(ok)

    def call(service, operation, **kwargs):
        try:
            return getattr(client(service), operation)(**kwargs)
        except Exception as exc:
            # Driver messages may contain request details. Only emit a bounded error code.
            code = getattr(exc, 'response', {}).get('Error', {}).get('Code', 'RequestFailed')
            if not re.fullmatch(r'[A-Za-z0-9_.-]{1,80}', str(code)):
                code = 'RequestFailed'
            blockers.append(f'{service}.{operation}: {code}')
            return {}

    try:
        count = int(params.get('DesiredCount', '2'))
    except ValueError:
        count = 0
    require(2 <= count <= 10, 'DesiredCount is between 2 and 10')
    try:
        cidr = ipaddress.ip_network(params['ClientCidr'], strict=True)
        valid_cidr = cidr.version == 4 and cidr.prefixlen > 0
    except ValueError:
        valid_cidr = False
    require(valid_cidr, 'ClientCidr is a bounded IPv4 network, not all IPv4 addresses')
    valid_url = bool(re.fullmatch(r'https://[A-Za-z0-9.-]+', params['PublicUrl']))
    url = urlsplit(params['PublicUrl']) if valid_url else urlsplit('')
    require(valid_url, 'PublicUrl is an HTTPS origin without a path or credentials')

    def region_arn(value, service):
        parts = value.split(':', 5)
        return len(parts) == 6 and parts[0] == 'arn' and parts[2] == service and parts[3] == region

    cert_arn = params['CertificateArn']
    if require(region_arn(cert_arn, 'acm'), 'CertificateArn belongs to the deployment region'):
        cert = call('acm', 'describe_certificate', CertificateArn=cert_arn).get('Certificate', {})
        require(cert.get('Status') == 'ISSUED', 'TLS certificate is issued')
        names = cert.get('SubjectAlternativeNames', [cert.get('DomainName', '')])
        require(valid_url and any(dns_matches(n, url.hostname or '') for n in names),
                'TLS certificate covers PublicUrl')

    secret_arn = params['SealingKeySecretArn']
    if require(region_arn(secret_arn, 'secretsmanager'), 'Sealing-key secret belongs to deployment region'):
        secret = call('secretsmanager', 'describe_secret', SecretId=secret_arn)
        require(bool(secret) and not secret.get('DeletedDate'), 'Sealing-key secret exists and is not scheduled for deletion')
        require(any('AWSCURRENT' in stages for stages in secret.get('VersionIdsToStages', {}).values()),
                'Sealing-key secret has a current version')
        key = secret.get('KmsKeyId')
        supplied_key = params.get('SealingKeyKmsArn', '')
        require(not key or key == 'alias/aws/secretsmanager' or key == supplied_key,
                'Customer-managed sealing-key KMS key matches SealingKeyKmsArn')
        warnings.append('Sealing-key value and decryption permission were not read or tested; verify it contains the existing base64 32-byte key.')

    image = re.fullmatch(r'(\d{12})\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com(?:\.cn)?/([a-z0-9._/-]+)@(sha256:[a-f0-9]{64})', params['ImageUri'])
    if require(bool(image), 'ImageUri is a private ECR image pinned by SHA-256 digest'):
        account, image_region, repository, digest = image.groups()
        if require(image_region == region, 'ECR image belongs to deployment region'):
            details = call('ecr', 'describe_images', registryId=account, repositoryName=repository,
                           imageIds=[{'imageDigest': digest}]).get('imageDetails', [])
            require(any(d.get('imageDigest') == digest for d in details), 'Pinned image exists in ECR')
            warnings.append('Image architecture and runtime health require the linux/amd64 build and deployment smoke test.')

    topic = params['AlarmTopicArn']
    if require(region_arn(topic, 'sns'), 'Alarm topic belongs to deployment region'):
        response = call('sns', 'get_topic_attributes', TopicArn=topic)
        attrs = response.get('Attributes', {})
        require(int(attrs.get('SubscriptionsConfirmed', '0')) > 0, 'Alarm topic has at least one confirmed subscription')

    vpc = params['VpcId']
    if not require(bool(re.fullmatch(r'vpc-[a-f0-9]+', vpc)) and all(re.fullmatch(r'subnet-[a-f0-9]+', params[k]) for k in ('PrivateSubnetA', 'PrivateSubnetB')), 'VPC and subnet identifiers have valid syntax'):
        return {'status': 'BLOCKED', 'region': region, 'checks_passed': passed, 'blockers': blockers, 'warnings': warnings}
    vpcs = call('ec2', 'describe_vpcs', VpcIds=[vpc]).get('Vpcs', [])
    require(any(v.get('VpcId') == vpc and v.get('State') == 'available' for v in vpcs), 'VPC exists and is available')
    ids = [params['PrivateSubnetA'], params['PrivateSubnetB']]
    require(ids[0] != ids[1], 'Private subnets are distinct')
    subnets = call('ec2', 'describe_subnets', SubnetIds=sorted(set(ids))).get('Subnets', [])
    by_id = {s['SubnetId']: s for s in subnets}
    require(all(s in by_id for s in ids), 'Both private subnets exist')
    require(all(by_id.get(s, {}).get('VpcId') == vpc for s in ids), 'Private subnets belong to VpcId')
    require(len({by_id.get(s, {}).get('AvailabilityZone') for s in ids} - {None}) == 2,
            'Private subnets span two availability zones')
    require(all(not by_id.get(s, {}).get('MapPublicIpOnLaunch', True) for s in ids),
            'Private subnets do not assign public IP addresses by default')
    tables = call('ec2', 'describe_route_tables', Filters=[{'Name': 'vpc-id', 'Values': [vpc]}]).get('RouteTables', [])
    endpoints = call('ec2', 'describe_vpc_endpoints', Filters=[{'Name': 'vpc-id', 'Values': [vpc]}]).get('VpcEndpoints', [])
    nat_ids = set()
    for subnet_id in ids:
        subnet = by_id.get(subnet_id, {})
        explicit = [t for t in tables if any(a.get('SubnetId') == subnet_id for a in t.get('Associations', []))]
        selected = explicit or [t for t in tables if any(a.get('Main') for a in t.get('Associations', []))]
        if not require(len(selected) == 1, f'{subnet_id}: exactly one effective route table found'):
            continue
        table = selected[0]
        routes = [r for r in table.get('Routes', []) if r.get('State') == 'active']
        require(not any(r.get('GatewayId', '').startswith('igw-') for r in routes),
                f'{subnet_id}: no direct internet-gateway route')
        default = next((r for r in routes if r.get('DestinationCidrBlock') == '0.0.0.0/0'), {})
        if default.get('NatGatewayId'):
            nat_id = default['NatGatewayId']
            nat_ids.add(nat_id)
            nats = call('ec2', 'describe_nat_gateways', NatGatewayIds=[nat_id]).get('NatGateways', [])
            nat = nats[0] if nats else {}
            require(nat.get('State') == 'available', f'{subnet_id}: NAT gateway is available')
            nat_subnets = call('ec2', 'describe_subnets', SubnetIds=[nat['SubnetId']]).get('Subnets', []) if nat.get('SubnetId') else []
            require(bool(nat_subnets) and nat_subnets[0].get('AvailabilityZone') == subnet.get('AvailabilityZone'),
                    f'{subnet_id}: NAT gateway is in the same availability zone')
        else:
            for service in ('ecr.api', 'ecr.dkr', 'logs', 'secretsmanager'):
                require(any(e.get('State') == 'available' and e.get('ServiceName') == f'com.amazonaws.{region}.{service}'
                            and e.get('PrivateDnsEnabled') and subnet_id in e.get('SubnetIds', []) for e in endpoints),
                        f'{subnet_id}: private {service} endpoint is available in subnet')
            require(any(e.get('State') == 'available' and e.get('ServiceName') == f'com.amazonaws.{region}.s3'
                        and table['RouteTableId'] in e.get('RouteTableIds', []) for e in endpoints),
                    f'{subnet_id}: S3 gateway endpoint covers route table')
            warnings.append(f'{subnet_id}: endpoint-only networking does not establish external GitHub/model-provider access.')
    if len(nat_ids) == 1 and len(ids) == 2:
        warnings.append('Only one NAT gateway was found; inspect both subnet paths and endpoint coverage for AZ independence.')
    warnings.append('Route inspection does not test DNS, security-group/NACL reachability, NAT public routes, client VPN access, or account provisioning restrictions.')
    engine_version = params.get('DatabaseEngineVersion', '17.11')
    engines = call('rds', 'describe_db_engine_versions', Engine='postgres', EngineVersion=engine_version).get('DBEngineVersions', [])
    require(bool(re.fullmatch(r'17\.[0-9]+', engine_version)) and any(e.get('EngineVersion') == engine_version for e in engines), 'Selected PostgreSQL 17 version is available in deployment region')
    if engines:
        orderable = call('rds', 'describe_orderable_db_instance_options', Engine='postgres',
                         DBInstanceClass=params.get('DatabaseClass', 'db.t4g.medium'),
                         EngineVersion=engine_version, Vpc=True).get('OrderableDBInstanceOptions', [])
        require(any(o.get('EngineVersion') == engine_version and o.get('MultiAZCapable') for o in orderable),
                'Database class supports PostgreSQL 17 Multi-AZ in deployment region')
    return {'status': 'PASS' if not blockers else 'BLOCKED', 'region': region,
            'checks_passed': passed, 'blockers': blockers, 'warnings': warnings}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--region', required=True)
    parser.add_argument('--parameters', required=True)
    args = parser.parse_args(argv)
    try:
        with open(args.parameters) as handle:
            params = parameters(json.load(handle))
    except (OSError, ValueError) as exc:
        # A malformed JSON error can quote input, so never echo its source text.
        print(json.dumps({'status': 'BLOCKED', 'blockers': ['Invalid parameter file: expected unique supported CloudFormation parameter strings.']}))
        return 2
    try:
        import boto3
        from botocore.config import Config
    except ImportError:
        print(json.dumps({'status': 'BLOCKED', 'blockers': ['Install boto3 to run preflight.']}))
        return 2
    session = boto3.Session(region_name=args.region)
    clients = {}
    def client(service):
        if service not in clients:
            clients[service] = session.client(service, config=Config(connect_timeout=5, read_timeout=10, retries={'max_attempts': 2}))
        return clients[service]
    result = check(params, args.region, client)
    print(json.dumps(result, indent=2))
    return 0 if result['status'] == 'PASS' else 1


if __name__ == '__main__':
    sys.exit(main())
