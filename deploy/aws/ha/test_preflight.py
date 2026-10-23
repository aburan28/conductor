"""Read-only contract and failure-mode tests; no AWS account is needed."""
import copy
import json
import unittest
from types import SimpleNamespace

from preflight import check, dns_matches, parameters


REGION = 'us-west-2'
DIGEST = 'sha256:' + 'a' * 64
PARAMS = {
    'VpcId': 'vpc-abc', 'PrivateSubnetA': 'subnet-aaa', 'PrivateSubnetB': 'subnet-bbb',
    'ImageUri': f'123456789012.dkr.ecr.{REGION}.amazonaws.com/conductor@{DIGEST}',
    'CertificateArn': f'arn:aws:acm:{REGION}:123456789012:certificate/abc',
    'PublicUrl': 'https://conductor.example.com', 'ClientCidr': '10.0.0.0/16',
    'SealingKeySecretArn': f'arn:aws:secretsmanager:{REGION}:123456789012:secret:conductor-key-abc',
    'AlarmTopicArn': f'arn:aws:sns:{REGION}:123456789012:alarms',
}


class FakeAWS:
    def __init__(self):
        self.calls = []
        self.cert_names = ['*.example.com']
        self.secret_deleted = False
        self.secret_key = None
        self.image_exists = True
        self.subnets = {
            'subnet-aaa': self.subnet('subnet-aaa', 'us-west-2a'),
            'subnet-bbb': self.subnet('subnet-bbb', 'us-west-2b'),
            'subnet-111': self.subnet('subnet-111', 'us-west-2a'),
            'subnet-222': self.subnet('subnet-222', 'us-west-2b'),
        }
        self.routes = [self.route('subnet-aaa', 'rtb-aaa', 'nat-aaa'), self.route('subnet-bbb', 'rtb-bbb', 'nat-bbb')]
        self.fail = None
        self.endpoints = []

    @staticmethod
    def subnet(identifier, az):
        return {'SubnetId': identifier, 'VpcId': 'vpc-abc', 'AvailabilityZone': az, 'MapPublicIpOnLaunch': False}

    @staticmethod
    def route(subnet, table, nat):
        return {'RouteTableId': table, 'Associations': [{'SubnetId': subnet}],
                'Routes': [{'DestinationCidrBlock': '0.0.0.0/0', 'State': 'active', 'NatGatewayId': nat}]}

    def client(self, service):
        return SimpleNamespace(**{name: self.function(service, name) for name in (
            'describe_certificate', 'describe_secret', 'describe_images', 'get_topic_attributes',
            'describe_vpcs', 'describe_subnets', 'describe_route_tables', 'describe_vpc_endpoints',
            'describe_nat_gateways', 'describe_db_engine_versions', 'describe_orderable_db_instance_options')})

    def function(self, service, name):
        def response(**kwargs):
            self.calls.append((service, name, kwargs))
            if name == self.fail:
                raise RuntimeError('SECRET-DO-NOT-PRINT')
            if name == 'describe_certificate':
                return {'Certificate': {'Status': 'ISSUED', 'SubjectAlternativeNames': self.cert_names}}
            if name == 'describe_secret':
                value = {'VersionIdsToStages': {'version': ['AWSCURRENT']}}
                if self.secret_deleted:
                    value['DeletedDate'] = 'soon'
                if self.secret_key:
                    value['KmsKeyId'] = self.secret_key
                return value
            if name == 'describe_images':
                return {'imageDetails': [{'imageDigest': DIGEST}] if self.image_exists else []}
            if name == 'get_topic_attributes':
                return {'Attributes': {'SubscriptionsConfirmed': '1'}}
            if name == 'describe_vpcs':
                return {'Vpcs': [{'VpcId': 'vpc-abc', 'State': 'available'}]}
            if name == 'describe_subnets':
                return {'Subnets': [self.subnets[s] for s in kwargs['SubnetIds'] if s in self.subnets]}
            if name == 'describe_route_tables':
                return {'RouteTables': self.routes}
            if name == 'describe_vpc_endpoints':
                return {'VpcEndpoints': self.endpoints}
            if name == 'describe_nat_gateways':
                nat = kwargs['NatGatewayIds'][0]
                return {'NatGateways': [{'State': 'available', 'SubnetId': 'subnet-111' if nat == 'nat-aaa' else 'subnet-222'}]}
            if name == 'describe_db_engine_versions':
                return {'DBEngineVersions': [{'EngineVersion': '17.11'}]}
            if name == 'describe_orderable_db_instance_options':
                return {'OrderableDBInstanceOptions': [{'EngineVersion': '17.11', 'MultiAZCapable': True}]}
            raise AssertionError(name)
        return response


class PreflightTests(unittest.TestCase):
    def setUp(self):
        self.aws = FakeAWS()
        self.params = copy.deepcopy(PARAMS)

    def run_check(self):
        return check(self.params, REGION, self.aws.client)

    def test_healthy_independent_nat_paths_and_read_only_calls(self):
        result = self.run_check()
        self.assertEqual(result['status'], 'PASS', result)
        self.assertTrue(all(name.startswith('describe_') or name == 'get_topic_attributes' for _, name, _ in self.aws.calls))
        self.assertNotIn('get_secret_value', str(self.aws.calls))

    def test_same_az_blocks(self):
        self.aws.subnets['subnet-bbb']['AvailabilityZone'] = 'us-west-2a'
        self.assertIn('Private subnets span two availability zones', self.run_check()['blockers'])

    def test_wrong_vpc_blocks(self):
        self.aws.subnets['subnet-bbb']['VpcId'] = 'vpc-def'
        self.assertIn('Private subnets belong to VpcId', self.run_check()['blockers'])

    def test_shared_nat_blocks_independence(self):
        self.aws.routes[1]['Routes'][0]['NatGatewayId'] = 'nat-aaa'
        result = self.run_check()
        self.assertEqual(result['status'], 'BLOCKED')
        self.assertIn('subnet-bbb: NAT gateway is in the same availability zone', result['blockers'])

    def test_no_route_or_endpoints_blocks(self):
        self.aws.routes[1]['Routes'] = []
        result = self.run_check()
        self.assertEqual(result['status'], 'BLOCKED')
        self.assertIn('subnet-bbb: S3 gateway endpoint covers route table', result['blockers'])

    def test_public_subnet_blocks(self):
        self.aws.routes[0]['Routes'] = [{'GatewayId': 'igw-aaa', 'State': 'active', 'DestinationCidrBlock': '0.0.0.0/0'}]
        self.assertIn('subnet-aaa: no direct internet-gateway route', self.run_check()['blockers'])

    def test_certificate_mismatch_blocks(self):
        self.aws.cert_names = ['*.other.example.com']
        self.assertIn('TLS certificate covers PublicUrl', self.run_check()['blockers'])

    def test_wildcard_is_exactly_one_label(self):
        self.assertTrue(dns_matches('*.example.com', 'conductor.example.com'))
        self.assertFalse(dns_matches('*.example.com', 'a.b.example.com'))
        self.assertFalse(dns_matches('*.example.com', 'example.com'))

    def test_cross_region_certificate_not_queried(self):
        self.params['CertificateArn'] = self.params['CertificateArn'].replace(REGION, 'us-east-1')
        self.assertEqual(self.run_check()['status'], 'BLOCKED')
        self.assertFalse(any(name == 'describe_certificate' for _, name, _ in self.aws.calls))

    def test_missing_digest_or_secret_blocks(self):
        self.aws.image_exists = False
        self.aws.secret_deleted = True
        result = self.run_check()
        self.assertIn('Pinned image exists in ECR', result['blockers'])
        self.assertIn('Sealing-key secret exists and is not scheduled for deletion', result['blockers'])

    def test_custom_key_requires_matching_permission_input(self):
        self.aws.secret_key = 'arn:aws:kms:us-west-2:123456789012:key/abc'
        self.assertEqual(self.run_check()['status'], 'BLOCKED')
        self.params['SealingKeyKmsArn'] = self.aws.secret_key
        self.assertEqual(self.run_check()['status'], 'PASS')

    def test_sdk_errors_never_echo_exception_text(self):
        self.aws.fail = 'describe_secret'
        output = json.dumps(self.run_check())
        self.assertNotIn('SECRET-DO-NOT-PRINT', output)
        self.assertIn('RequestFailed', output)

    def test_parameter_input_rejects_duplicate_unknown_and_use_previous(self):
        rows = [{'ParameterKey': k, 'ParameterValue': v} for k, v in PARAMS.items()]
        self.assertEqual(parameters(rows), PARAMS)
        for invalid in (rows + [rows[0]], rows + [{'ParameterKey': 'Token', 'ParameterValue': 'secret'}],
                        [{'ParameterKey': 'VpcId', 'UsePreviousValue': True}]):
            with self.assertRaises(ValueError):
                parameters(invalid)

    def test_selected_database_version_must_exist(self):
        self.params['DatabaseEngineVersion'] = '17.99'
        self.assertIn('Selected PostgreSQL 17 version is available in deployment region', self.run_check()['blockers'])

    def test_complete_endpoint_paths_warn_about_external_connectivity(self):
        for table in self.aws.routes:
            table['Routes'] = []
        for service in ('ecr.api', 'ecr.dkr', 'logs', 'secretsmanager'):
            self.aws.endpoints.append({'State': 'available', 'ServiceName': f'com.amazonaws.{REGION}.{service}',
                                       'PrivateDnsEnabled': True, 'SubnetIds': ['subnet-aaa', 'subnet-bbb']})
        self.aws.endpoints.append({'State': 'available', 'ServiceName': f'com.amazonaws.{REGION}.s3',
                                   'RouteTableIds': ['rtb-aaa', 'rtb-bbb']})
        result = self.run_check()
        self.assertEqual(result['status'], 'PASS', result)
        self.assertTrue(any('external GitHub/model-provider' in w for w in result['warnings']))

    def test_malformed_url_is_a_reported_blocker(self):
        self.params['PublicUrl'] = 'https://[invalid'
        self.assertEqual(self.run_check()['status'], 'BLOCKED')


if __name__ == '__main__':
    unittest.main()
