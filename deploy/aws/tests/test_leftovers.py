import contextlib
from datetime import datetime, timezone
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from loader import AWS, completed, load

leftovers = load('leftovers')
common = sys.modules['common']

ACCOUNT = '123456789012'
REGION = 'ap-southeast-1'
VPC = 'vpc-0123456789abcdef0'
OTHER_VPC = 'vpc-0fedcba9876543210'
TAGS = [{'Key': 'Project', 'Value': 'gateflow'}, {'Key': 'Environment', 'Value': 's3'}]
DB_ARN = f'arn:aws:rds:{REGION}:{ACCOUNT}:db:gateflow-s3'
SECRET_ARN = f'arn:aws:secretsmanager:{REGION}:{ACCOUNT}:secret:rds!db-00000000-aaaa-bbbb-cccc-000000000000-AbCdEf'
GONE_SECRET_ARN = f'arn:aws:secretsmanager:{REGION}:{ACCOUNT}:secret:rds!db-11111111-aaaa-bbbb-cccc-000000000000-GhIjKl'
LB_ARN = f'arn:aws:elasticloadbalancing:{REGION}:{ACCOUNT}:loadbalancer/app/k8s-gateflow-gf-0123456789/0123456789abcdef'
TG_ARN = f'arn:aws:elasticloadbalancing:{REGION}:{ACCOUNT}:targetgroup/k8s-gateflow-gfgatewa-0123456789/0123456789abcdef'
OTHER_LB = f'arn:aws:elasticloadbalancing:{REGION}:{ACCOUNT}:loadbalancer/app/unrelated/fedcba9876543210'
INVENTORY = {'kind': 'gateflow-teardown-inventory', 'cluster_names': ['gateflow-s3'], 'vpc_ids': [VPC],
             'security_group_ids': ['sg-0aaaaaaaaaaaaaaa1'], 'volume_ids': ['vol-0aaaaaaaaaaaaaaa1'],
             'db_instance_arns': [DB_ARN], 'secret_arns': [SECRET_ARN, GONE_SECRET_ARN]}

CLEAN = {
    ('eks', 'list-clusters'): {'clusters': []},
    ('ec2', 'describe-instances'): {'Reservations': []},
    ('rds', 'describe-db-instances'): {'DBInstances': []},
    ('rds', 'describe-db-snapshots'): {'DBSnapshots': []},
    ('rds', 'describe-db-instance-automated-backups'): {'DBInstanceAutomatedBackups': []},
    ('ec2', 'describe-volumes'): {'Volumes': []},
    ('ec2', 'describe-snapshots'): {'Snapshots': []},
    ('ec2', 'describe-network-interfaces'): {'NetworkInterfaces': []},
    ('ec2', 'describe-nat-gateways'): {'NatGateways': []},
    ('ec2', 'describe-addresses'): {'Addresses': []},
    ('elbv2', 'describe-load-balancers'): {'LoadBalancers': []},
    ('elbv2', 'describe-target-groups'): {'TargetGroups': []},
    ('elbv2', 'describe-tags'): {'TagDescriptions': []},
    ('ec2', 'describe-security-groups'): {'SecurityGroups': []},
    ('ec2', 'describe-subnets'): {'Subnets': []},
    ('ec2', 'describe-vpcs'): {'Vpcs': []},
    ('logs', 'describe-log-groups'): {'logGroups': []},
    ('secretsmanager', 'list-secrets'): {'SecretList': []},
    ('s3api', 'list-buckets'): {'Buckets': [{'Name': 'gateflow-tfstate-example'}]},
}

DIRTY = CLEAN | {
    ('eks', 'list-clusters'): {'clusters': ['gateflow-s3']},
    ('ec2', 'describe-instances'): {'Reservations': [{'Instances': [
        {'InstanceId': 'i-0aaaaaaaaaaaaaaa1', 'State': {'Name': 'running'}, 'VpcId': OTHER_VPC, 'Tags': TAGS},
        {'InstanceId': 'i-0aaaaaaaaaaaaaaa2', 'State': {'Name': 'running'}, 'VpcId': OTHER_VPC,
         'Tags': [{'Key': 'eks:cluster-name', 'Value': 'gateflow-s3'}]},
        {'InstanceId': 'i-0bbbbbbbbbbbbbbb9', 'State': {'Name': 'running'}, 'VpcId': OTHER_VPC, 'Tags': []}]}]},
    ('rds', 'describe-db-instances'): {'DBInstances': [
        {'DBInstanceIdentifier': 'gateflow-s3', 'DBInstanceStatus': 'deleting', 'DBInstanceArn': DB_ARN},
        {'DBInstanceIdentifier': 'unrelated-db', 'DBInstanceStatus': 'available'}]},
    ('rds', 'describe-db-snapshots'): {'DBSnapshots': [
        {'DBSnapshotIdentifier': 'gateflow-s3-final-20261017', 'DBInstanceIdentifier': 'gateflow-s3',
         'SnapshotType': 'manual', 'Status': 'available'},
        {'DBSnapshotIdentifier': 'rds:gateflow-s3-2026-10-17-03-00', 'DBInstanceIdentifier': 'gateflow-s3',
         'SnapshotType': 'automated', 'Status': 'available'},
        {'DBSnapshotIdentifier': 'rds:unrelated-2026-10-17', 'DBInstanceIdentifier': 'unrelated-db',
         'SnapshotType': 'automated', 'Status': 'available'}]},
    ('rds', 'describe-db-instance-automated-backups'): {'DBInstanceAutomatedBackups': [
        {'DBInstanceIdentifier': 'gateflow-s3', 'DbiResourceId': 'db-ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'Status': 'retained'}]},
    ('ec2', 'describe-volumes'): {'Volumes': [
        {'VolumeId': 'vol-0aaaaaaaaaaaaaaa1', 'State': 'in-use', 'Attachments': [{'InstanceId': 'i-0bbbbbbbbbbbbbbb9'}]},
        {'VolumeId': 'vol-0aaaaaaaaaaaaaaa2', 'State': 'in-use',
         'Tags': [{'Key': 'kubernetes.io/cluster/gateflow-s3', 'Value': 'owned'}]},
        {'VolumeId': 'vol-0ccccccccccccccc3', 'State': 'available'},
        {'VolumeId': 'vol-0bbbbbbbbbbbbbbb9', 'State': 'in-use'}]},
    ('ec2', 'describe-snapshots'): {'Snapshots': [
        {'SnapshotId': 'snap-0aaaaaaaaaaaaaaa1', 'VolumeId': 'vol-0aaaaaaaaaaaaaaa1', 'State': 'completed'},
        {'SnapshotId': 'snap-0bbbbbbbbbbbbbbb9', 'VolumeId': 'vol-0bbbbbbbbbbbbbbb9', 'State': 'completed'}]},
    ('ec2', 'describe-network-interfaces'): {'NetworkInterfaces': [
        {'NetworkInterfaceId': 'eni-0aaaaaaaaaaaaaaa1', 'VpcId': VPC, 'Status': 'available'},
        {'NetworkInterfaceId': 'eni-0aaaaaaaaaaaaaaa2', 'VpcId': OTHER_VPC, 'Status': 'in-use',
         'Description': 'Amazon EKS gateflow-s3'},
        {'NetworkInterfaceId': 'eni-0bbbbbbbbbbbbbbb9', 'VpcId': OTHER_VPC, 'Status': 'in-use'}]},
    ('ec2', 'describe-nat-gateways'): {'NatGateways': [
        {'NatGatewayId': 'nat-0aaaaaaaaaaaaaaa1', 'VpcId': VPC, 'State': 'deleting'},
        {'NatGatewayId': 'nat-0aaaaaaaaaaaaaaa2', 'VpcId': VPC, 'State': 'deleted'}]},
    ('ec2', 'describe-addresses'): {'Addresses': [
        {'AllocationId': 'eipalloc-0aaaaaaaaaaaaaaa1', 'PublicIp': '203.0.113.10'},
        {'AllocationId': 'eipalloc-0aaaaaaaaaaaaaaa2', 'AssociationId': 'eipassoc-1', 'Tags': TAGS},
        {'AllocationId': 'eipalloc-0bbbbbbbbbbbbbbb9', 'AssociationId': 'eipassoc-2'}]},
    ('elbv2', 'describe-load-balancers'): {'LoadBalancers': [
        {'LoadBalancerArn': LB_ARN, 'LoadBalancerName': 'k8s-gateflow-gf-0123456789', 'VpcId': OTHER_VPC,
         'Type': 'application', 'State': {'Code': 'active'}},
        {'LoadBalancerArn': OTHER_LB, 'LoadBalancerName': 'unrelated', 'VpcId': OTHER_VPC, 'Type': 'network'}]},
    ('elbv2', 'describe-target-groups'): {'TargetGroups': [
        {'TargetGroupArn': TG_ARN, 'TargetGroupName': 'k8s-gateflow-gfgatewa-0123456789', 'VpcId': VPC}]},
    ('elbv2', 'describe-tags'): {'TagDescriptions': [{'ResourceArn': LB_ARN, 'Tags': TAGS}]},
    ('ec2', 'describe-security-groups'): {'SecurityGroups': [
        {'GroupId': 'sg-0aaaaaaaaaaaaaaa1', 'GroupName': 'eks-cluster-sg', 'VpcId': OTHER_VPC},
        {'GroupId': 'sg-0aaaaaaaaaaaaaaa2', 'GroupName': 'k8s-traffic-gateflows3', 'VpcId': OTHER_VPC,
         'Tags': [{'Key': 'elbv2.k8s.aws/cluster', 'Value': 'gateflow-s3'}]},
        {'GroupId': 'sg-0bbbbbbbbbbbbbbb9', 'GroupName': 'default', 'VpcId': OTHER_VPC}]},
    ('ec2', 'describe-subnets'): {'Subnets': [{'SubnetId': 'subnet-0aaaaaaaaaaaaaaa1', 'VpcId': VPC,
                                              'State': 'available'}]},
    ('ec2', 'describe-vpcs'): {'Vpcs': [{'VpcId': VPC, 'State': 'available',
                                        'Tags': [{'Key': 'Name', 'Value': 'gateflow-s3'}]},
                                       {'VpcId': OTHER_VPC, 'State': 'available'}]},
    ('logs', 'describe-log-groups'): {'logGroups': [{'logGroupName': '/aws/eks/gateflow-s3/cluster'},
                                                    {'logGroupName': '/aws/lambda/unrelated'}]},
    ('secretsmanager', 'list-secrets'): {'SecretList': [
        {'ARN': f'arn:aws:secretsmanager:{REGION}:{ACCOUNT}:secret:rds!db-22222222-x', 'Name': 'rds!db-22222222-x',
         'Tags': [{'Key': 'aws:rds:primaryDBInstanceArn', 'Value': DB_ARN}]},
        {'ARN': f'arn:aws:secretsmanager:{REGION}:{ACCOUNT}:secret:unrelated', 'Name': 'unrelated'}]},
    ('secretsmanager', 'describe-secret'): {'existing': [SECRET_ARN]},
    ('s3api', 'list-buckets'): {'Buckets': [{'Name': 'gateflow-tfstate-example'}, {'Name': 'gateflow-s3-evidence'},
                                            {'Name': 'unrelated-bucket'}]},
}

EXPECTED = {
    ('eks-cluster', 'gateflow-s3'),
    ('ec2-instance', 'i-0aaaaaaaaaaaaaaa1'), ('ec2-instance', 'i-0aaaaaaaaaaaaaaa2'),
    ('rds-instance', 'gateflow-s3'), ('rds-snapshot-manual', 'gateflow-s3-final-20261017'),
    ('rds-snapshot-automated', 'rds:gateflow-s3-2026-10-17-03-00'),
    ('rds-retained-backup', 'db-ABCDEFGHIJKLMNOPQRSTUVWXYZ'),
    ('ebs-volume', 'vol-0aaaaaaaaaaaaaaa1'), ('ebs-volume', 'vol-0aaaaaaaaaaaaaaa2'),
    ('ebs-volume', 'vol-0ccccccccccccccc3'), ('ebs-snapshot', 'snap-0aaaaaaaaaaaaaaa1'),
    ('network-interface', 'eni-0aaaaaaaaaaaaaaa1'), ('network-interface', 'eni-0aaaaaaaaaaaaaaa2'),
    ('nat-gateway', 'nat-0aaaaaaaaaaaaaaa1'),
    ('elastic-ip', 'eipalloc-0aaaaaaaaaaaaaaa1'), ('elastic-ip', 'eipalloc-0aaaaaaaaaaaaaaa2'),
    ('load-balancer-application', LB_ARN), ('target-group', TG_ARN),
    ('security-group', 'sg-0aaaaaaaaaaaaaaa1'), ('security-group', 'sg-0aaaaaaaaaaaaaaa2'),
    ('subnet', 'subnet-0aaaaaaaaaaaaaaa1'), ('vpc', VPC),
    ('log-group', '/aws/eks/gateflow-s3/cluster'),
    ('secret', f'arn:aws:secretsmanager:{REGION}:{ACCOUNT}:secret:rds!db-22222222-x'), ('secret', SECRET_ARN),
    ('s3-bucket', 'gateflow-s3-evidence'),
}


class FakeAWS:
    """Answers aws CLI calls from a table keyed by (service, operation)."""

    def __init__(self, responses, failures=None, rounds=None):
        self.responses, self.failures = responses, failures or {}
        self.rounds = rounds  # list of response tables, one per check round, the last repeated
        self.commands = []

    def table(self):
        if not self.rounds:
            return self.responses
        done = sum(1 for command in self.commands if command[1:3] == ['eks', 'list-clusters']) - 1
        return self.rounds[min(max(done, 0), len(self.rounds) - 1)]

    def __call__(self, command, **kwargs):
        self.commands.append(list(command))
        if command[0] == 'git':
            return completed(command, 1)
        assert command[0] == 'aws' and command[-4:] == ['--region', REGION, '--output', 'json'], command
        service, operation = command[1], command[2]
        if (service, operation) in self.failures:
            return completed(command, 254, stderr=self.failures[(service, operation)])
        if (service, operation) == ('secretsmanager', 'describe-secret'):
            arn = command[command.index('--secret-id') + 1]
            if arn in self.table().get((service, operation), {}).get('existing', []):
                return completed(command, stdout=json.dumps({'ARN': arn, 'Name': 'rds!db-00000000'}))
            return completed(command, 254, stderr='An error occurred (ResourceNotFoundException) when calling '
                                                  'the DescribeSecret operation: not found')
        response = self.table().get((service, operation))
        if response is None:
            raise AssertionError(f'unexpected command {command}')
        if (service, operation) == ('rds', 'describe-db-snapshots'):
            kind = command[command.index('--snapshot-type') + 1]
            response = {'DBSnapshots': [item for item in response['DBSnapshots'] if item['SnapshotType'] == kind]}
        return completed(command, stdout=json.dumps(response))


class Clock:
    def __init__(self):
        self.now, self.sleeps = 0.0, []

    def __call__(self):
        return self.now

    def sleep(self, seconds):
        self.sleeps.append(seconds)
        self.now += seconds


class LeftoversTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.dir = Path(directory.name)
        self.inventory = self.dir/'inventory.json'
        self.inventory.write_text(json.dumps(INVENTORY), encoding='utf-8')

    def run_main(self, runner, *argv, clock=None):
        clock = clock or Clock()
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = leftovers.main(list(argv), runner=runner, sleep=clock.sleep, clock=clock,
                                  now=datetime(2026, 10, 17, 9, 0, tzinfo=timezone.utc))
        return code, out.getvalue(), err.getvalue()

    def check(self, runner, *extra, clock=None):
        return self.run_main(runner, 'check', '--inventory', str(self.inventory),
                             '--state-bucket', 'gateflow-tfstate-example', *extra, clock=clock)

    def test_no_leftovers_exits_zero(self):
        code, out, _ = self.check(FakeAWS(CLEAN))
        self.assertEqual(code, 0)
        self.assertIn('no billable leftovers found', out)
        self.assertIn('retained: s3-bucket gateflow-tfstate-example', out)

    def test_every_resource_type_is_judged(self):
        runner = FakeAWS(DIRTY)
        code, out, _ = self.check(runner, '--json')
        self.assertEqual(code, 1)
        result = json.loads(out)
        found = {(item['type'], item['id']) for item in result['leftovers']}
        self.assertEqual(found, EXPECTED)
        reasons = {(item['type'], item['id']): item['reason'] for item in result['leftovers']}
        self.assertIn('inventory', reasons[('security-group', 'sg-0aaaaaaaaaaaaaaa1')])
        self.assertIn(f'in {VPC}', reasons[('network-interface', 'eni-0aaaaaaaaaaaaaaa1')])
        self.assertIn('mentions cluster gateflow-s3', reasons[('network-interface', 'eni-0aaaaaaaaaaaaaaa2')])
        self.assertIn('tags Project=gateflow,Environment=s3', reasons[('load-balancer-application', LB_ARN)])
        self.assertIn('cluster log group gateflow-s3', reasons[('log-group', '/aws/eks/gateflow-s3/cluster')])
        self.assertIn('unassociated', reasons[('elastic-ip', 'eipalloc-0aaaaaaaaaaaaaaa1')])
        self.assertIn('unattached', reasons[('ebs-volume', 'vol-0ccccccccccccccc3')])
        self.assertEqual(result['unchecked'], [])
        for command in runner.commands:
            self.assertTrue(command[2].startswith(('describe-', 'list-')), command)

    def test_state_bucket_is_only_retained(self):
        code, out, _ = self.check(FakeAWS(DIRTY), '--json')
        result = json.loads(out)
        self.assertEqual([item['id'] for item in result['retained']], ['gateflow-tfstate-example'])
        self.assertNotIn('gateflow-tfstate-example', [item['id'] for item in result['leftovers']])
        bucket = DIRTY | {('s3api', 'list-buckets'): {'Buckets': [{'Name': 'gateflow-s3-tfstate'}]}}
        code, out, _ = self.run_main(FakeAWS(bucket), 'check', '--state-bucket', 'gateflow-s3-tfstate', '--json')
        result = json.loads(out)
        self.assertEqual([item['id'] for item in result['retained']], ['gateflow-s3-tfstate'])
        self.assertNotIn('gateflow-s3-tfstate', [item['id'] for item in result['leftovers']])

    def test_table_output_lists_leftovers(self):
        code, out, _ = self.check(FakeAWS(DIRTY))
        self.assertEqual(code, 1)
        self.assertTrue(out.startswith('TYPE'))
        self.assertIn('nat-gateway', out)
        self.assertNotIn('no billable leftovers found', out)

    def test_failed_query_is_unchecked_and_exits_two(self):
        failures = {('rds', 'describe-db-instances'): 'An error occurred (AccessDenied) when calling DescribeDBInstances'}
        code, out, _ = self.check(FakeAWS(CLEAN, failures))
        self.assertEqual(code, 2)
        self.assertIn('unchecked: rds: aws rds describe-db-instances exited with 254', out)
        self.assertNotIn('no billable leftovers found', out)
        code, out, _ = self.check(FakeAWS(DIRTY, failures), '--json')
        result = json.loads(out)
        self.assertEqual(code, 2)
        self.assertEqual([item['check'] for item in result['unchecked']], ['rds'])
        self.assertIn(('nat-gateway', 'nat-0aaaaaaaaaaaaaaa1'),
                      {(item['type'], item['id']) for item in result['leftovers']})

    def test_missing_aws_cli_is_unchecked(self):
        def runner(command, **kwargs):
            raise FileNotFoundError(2, 'No such file or directory')
        code, out, _ = self.check(runner)
        self.assertEqual(code, 2)
        self.assertEqual(out.count('unchecked:'), len(leftovers.CHECKS) + 1)

    def test_wait_polls_until_clean(self):
        runner = FakeAWS(None, rounds=[DIRTY, DIRTY, CLEAN])
        clock = Clock()
        code, out, err = self.check(runner, '--wait', '600', '--interval', '20', clock=clock)
        self.assertEqual(code, 0)
        self.assertEqual(clock.sleeps, [20, 20])
        self.assertIn('no billable leftovers found', out)
        self.assertIn('checking again in 20s', err)

    def test_wait_gives_up_with_the_last_result(self):
        clock = Clock()
        code, out, _ = self.check(FakeAWS(DIRTY), '--wait', '50', '--interval', '20', clock=clock)
        self.assertEqual(code, 1)
        self.assertEqual(clock.sleeps, [20, 20, 10])
        self.assertIn('nat-gateway', out)

    def test_check_without_wait_runs_once(self):
        clock = Clock()
        code, _, err = self.run_main(FakeAWS(DIRTY), 'check', clock=clock)
        self.assertEqual(code, 1)
        self.assertEqual(clock.sleeps, [])
        self.assertIn('no --inventory', err)

    def test_check_refuses_a_file_that_is_not_an_inventory(self):
        self.inventory.write_text(json.dumps({'vpc_ids': [VPC]}), encoding='utf-8')
        code, _, err = self.check(FakeAWS(CLEAN))
        self.assertEqual(code, 2)
        self.assertIn('not a file written by', err)


INVENTORY_AWS = {
    ('eks', 'describe-cluster'): {'cluster': {'arn': f'arn:aws:eks:{REGION}:{ACCOUNT}:cluster/gateflow-s3',
                                              'resourcesVpcConfig': {'clusterSecurityGroupId': 'sg-0ccccccccccccccc1'}}},
    ('ec2', 'describe-instances'): {'Reservations': [{'Instances': [{
        'InstanceId': 'i-0aaaaaaaaaaaaaaa1', 'BlockDeviceMappings': [{'Ebs': {'VolumeId': 'vol-0aaaaaaaaaaaaaaa1'}}],
        'NetworkInterfaces': [{'NetworkInterfaceId': 'eni-0aaaaaaaaaaaaaaa1'}]}]}]},
    ('ec2', 'describe-security-groups'): {'SecurityGroups': [{'GroupId': 'sg-0aaaaaaaaaaaaaaa1'}]},
    ('ec2', 'describe-network-interfaces'): {'NetworkInterfaces': [{'NetworkInterfaceId': 'eni-0aaaaaaaaaaaaaaa2'}]},
    ('ec2', 'describe-nat-gateways'): {'NatGateways': [{'NatGatewayId': 'nat-0aaaaaaaaaaaaaaa1', 'NatGatewayAddresses': [
        {'AllocationId': 'eipalloc-0aaaaaaaaaaaaaaa1'}]}]},
    ('ec2', 'describe-volumes'): {'Volumes': [{'VolumeId': 'vol-0aaaaaaaaaaaaaaa2'}]},
    ('elbv2', 'describe-load-balancers'): {'LoadBalancers': [{'LoadBalancerArn': LB_ARN, 'VpcId': VPC},
                                                             {'LoadBalancerArn': OTHER_LB, 'VpcId': OTHER_VPC}]},
    ('elbv2', 'describe-target-groups'): {'TargetGroups': [{'TargetGroupArn': TG_ARN, 'VpcId': VPC}]},
    ('logs', 'describe-log-groups'): {'logGroups': [{'logGroupName': '/aws/eks/gateflow-s3/cluster'}]},
    ('rds', 'describe-db-instances'): {'DBInstances': [{'DBInstanceArn': DB_ARN, 'DbiResourceId': 'db-ABCDEFGHIJKLMNOP',
                                                        'MasterUserSecret': {'SecretArn': SECRET_ARN}}]},
}
TF_OUTPUTS = {'vpc_id': VPC, 'cluster_name': 'gateflow-s3', 'public_subnet_ids': ['subnet-0aaaaaaaaaaaaaaa1'],
              'private_subnet_ids': ['subnet-0aaaaaaaaaaaaaaa2'], 'service_security_group_id': 'sg-0ddddddddddddddd1',
              'generator_security_group_id': 'sg-0ddddddddddddddd2',
              'controller_role_arn': f'arn:aws:iam::{ACCOUNT}:role/gateflow-s3-alb-controller',
              'database_address': f'gateflow-s3.abcdefghijkl.{REGION}.rds.amazonaws.com', 'database_port': 5432,
              'database_name': 'gateflow', 'master_secret_arn': SECRET_ARN}


class InventoryTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.dir = Path(directory.name)
        self.outputs = self.dir/'outputs.json'
        self.outputs.write_text(json.dumps({key: {'sensitive': False, 'type': 'x', 'value': value}
                                            for key, value in TF_OUTPUTS.items()}), encoding='utf-8')
        self.out = self.dir/'inventory.json'

    def run_inventory(self, runner, out=None):
        output = io.StringIO()
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(io.StringIO()):
            code = leftovers.main(['inventory', '--terraform-output', str(self.outputs), '--out', str(out or self.out)],
                                  runner=runner, now=datetime(2026, 10, 17, 9, 0, tzinfo=timezone.utc))
        return code, output.getvalue()

    def test_inventory_records_terraform_and_aws_identifiers(self):
        runner = FakeAWS(INVENTORY_AWS)
        code, out = self.run_inventory(runner)
        self.assertEqual(code, 0)
        inventory = json.loads(self.out.read_text(encoding='utf-8'))
        self.assertEqual(inventory['kind'], 'gateflow-teardown-inventory')
        self.assertEqual(inventory['vpc_ids'], [VPC])
        self.assertEqual(inventory['subnet_ids'], ['subnet-0aaaaaaaaaaaaaaa1', 'subnet-0aaaaaaaaaaaaaaa2'])
        self.assertEqual(inventory['security_group_ids'], ['sg-0aaaaaaaaaaaaaaa1', 'sg-0ccccccccccccccc1',
                                                           'sg-0ddddddddddddddd1', 'sg-0ddddddddddddddd2'])
        self.assertEqual(inventory['volume_ids'], ['vol-0aaaaaaaaaaaaaaa1', 'vol-0aaaaaaaaaaaaaaa2'])
        self.assertEqual(inventory['network_interface_ids'], ['eni-0aaaaaaaaaaaaaaa1', 'eni-0aaaaaaaaaaaaaaa2'])
        self.assertEqual(inventory['allocation_ids'], ['eipalloc-0aaaaaaaaaaaaaaa1'])
        self.assertEqual(inventory['load_balancer_arns'], [LB_ARN])
        self.assertEqual(inventory['target_group_arns'], [TG_ARN])
        self.assertEqual(inventory['log_group_names'], ['/aws/eks/gateflow-s3/cluster'])
        self.assertEqual(inventory['db_instance_identifiers'], ['gateflow-s3'])
        self.assertEqual(inventory['secret_arns'], [SECRET_ARN])
        self.assertEqual(inventory['unchecked'], [])
        self.assertIn(['--filters', f'Name=vpc-id,Values={VPC}'],
                      [command[3:5] for command in runner.commands if command[2] == 'describe-instances'])
        log_prefixes = [command[4] for command in runner.commands if command[2] == 'describe-log-groups']
        self.assertEqual(log_prefixes, ['/aws/eks/gateflow-s3/', '/aws/rds/instance/gateflow-s3/'])
        self.assertIn('inventory written', out)

    def test_inventory_feeds_check(self):
        self.assertEqual(self.run_inventory(FakeAWS(INVENTORY_AWS))[0], 0)
        responses = CLEAN | {('ec2', 'describe-volumes'): {'Volumes': [
            {'VolumeId': 'vol-0aaaaaaaaaaaaaaa1', 'State': 'in-use'}]}}
        output = io.StringIO()
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(io.StringIO()):
            code = leftovers.main(['check', '--inventory', str(self.out), '--json'], runner=FakeAWS(responses))
        self.assertEqual(code, 1)
        result = json.loads(output.getvalue())
        self.assertEqual([(item['type'], item['id'], item['reason']) for item in result['leftovers']],
                         [('ebs-volume', 'vol-0aaaaaaaaaaaaaaa1', 'inventory')])

    def test_failed_inventory_query_is_recorded_and_exits_two(self):
        failures = {('rds', 'describe-db-instances'): 'An error occurred (AccessDenied)'}
        code, out = self.run_inventory(FakeAWS(INVENTORY_AWS, failures))
        self.assertEqual(code, 2)
        inventory = json.loads(self.out.read_text(encoding='utf-8'))
        self.assertEqual([item['query'] for item in inventory['unchecked']], ['rds describe-db-instances'])
        self.assertEqual(inventory['secret_arns'], [SECRET_ARN])
        self.assertIn('unchecked: rds describe-db-instances', out)

    def test_inventory_refuses_a_path_git_does_not_ignore(self):
        inside = common.ROOT/'deploy'/'aws'/'tests'/'not-ignored-inventory.json'
        runner = FakeAWS(INVENTORY_AWS)
        code, _ = self.run_inventory(runner, out=inside)
        self.assertEqual(code, 2)
        self.assertFalse(inside.exists())
        self.assertEqual([command[0] for command in runner.commands], ['git'])

    def test_runs_as_a_script_beside_secrets_py(self):
        # deploy/aws/secrets.py shadows the stdlib module for scripts run from deploy/aws.
        result = subprocess.run([sys.executable, str(AWS/'leftovers.py'), 'check', '--help'], capture_output=True, text=True,
                                encoding='utf-8', cwd=self.dir)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('--state-bucket', result.stdout)


if __name__ == '__main__':
    unittest.main()
