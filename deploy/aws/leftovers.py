"""Record the S3 environment's AWS resources before teardown and find what is left after it.

Operator-run (bibi45c only), read-only: every AWS call is a describe or list call of
the signed-in aws CLI (`aws ... --output json`).

  inventory  Before `terraform destroy`: saves the identifiers of the environment, from
             the Terraform outputs and from AWS (VPC, subnets, security groups, EKS
             cluster, instances, volumes, network interfaces, NAT gateways and their
             Elastic IPs, load balancers, target groups, log groups, the RDS instance
             and the exact ARN of its managed master secret) to a JSON file.
  check      After destroy: lists resources still present that carry the
             Project/Environment tags or a cluster tag, belong to a recorded VPC or
             cluster, appear in the inventory, match the name prefix, or are billable on
             their own (EKS clusters, NAT gateways, unattached Elastic IPs and
             available EBS volumes). The state bucket is only ever listed as retained.

Exit codes of check: 0 no leftovers, 1 leftovers found, 2 some query failed (shown as
unchecked, never as absent). With --wait SECONDS it checks again every --interval
seconds until nothing is left or the time is up, because load balancers and network
interfaces are released with a delay.
"""
import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import subprocess
import sys
import time

from common import ENVIRONMENT, NAME_PREFIX, PROJECT, REGION, ROOT, OperatorError, require_ignored, \
    terraform_outputs, write_private

DEFAULT_INVENTORY = ROOT/'.local'/'cloud-day'/'teardown-inventory.json'
INVENTORY_KEYS = ('cluster_names', 'cluster_arns', 'vpc_ids', 'subnet_ids', 'security_group_ids', 'instance_ids',
                  'volume_ids', 'network_interface_ids', 'nat_gateway_ids', 'allocation_ids', 'load_balancer_arns',
                  'target_group_arns', 'log_group_names', 'db_instance_identifiers', 'db_instance_arns',
                  'db_resource_ids', 'secret_arns', 'role_arns')
LIVE_INSTANCE_STATES = 'pending,running,shutting-down,stopping,stopped'


class QueryError(Exception):
    pass


def aws_caller(runner, region):
    """Returns call(service, operation, *args) -> parsed JSON; raises QueryError on any failure."""
    env = os.environ | {'AWS_PAGER': ''}

    def call(service, operation, *args):
        command = ['aws', service, operation, *args, '--region', region, '--output', 'json']
        try:
            result = runner(command, capture_output=True, text=True, encoding='utf-8', errors='replace', env=env)
        except OSError as error:
            raise QueryError(f'aws could not start: {error.strerror}') from error
        if result.returncode:
            lines = (result.stderr or '').strip().splitlines()
            raise QueryError(f'aws {service} {operation} exited with {result.returncode}: '
                             f'{lines[-1] if lines else "no error output"}')
        try:
            return json.loads(result.stdout or '{}')
        except ValueError as error:
            raise QueryError(f'aws {service} {operation} did not return JSON') from error
    return call


def tag_map(resource):
    for key in ('Tags', 'TagSet', 'TagList'):
        tags = resource.get(key)
        if isinstance(tags, list):
            return {tag.get('Key'): tag.get('Value') for tag in tags if isinstance(tag, dict)}
        if isinstance(tags, dict):
            return dict(tags)
    return {}


class Context:
    """What links a resource to this environment: the inventory, tags, names and clusters."""

    def __init__(self, args, inventory):
        inventory = inventory or {}
        self.prefix, self.project, self.environment = args.name_prefix, args.project, args.environment
        self.state_bucket = getattr(args, 'state_bucket', None)
        self.known = {value for key in INVENTORY_KEYS for value in inventory.get(key, []) if value}
        self.vpcs = set(inventory.get('vpc_ids', []))
        self.clusters = set(inventory.get('cluster_names', [])) | {self.prefix}
        self.secret_arns = list(inventory.get('secret_arns', []))

    def reasons(self, ids=(), resource=None, vpc=None, names=(), contains=(), text=''):
        found = []
        if any(value in self.known for value in ids if value):
            found.append('inventory')
        tags = tag_map(resource or {})
        if tags.get('Project') == self.project and tags.get('Environment') == self.environment:
            found.append(f'tags Project={self.project},Environment={self.environment}')
        for cluster in self.clusters:
            if (f'kubernetes.io/cluster/{cluster}' in tags or cluster in {
                    tags.get('eks:cluster-name'), tags.get('aws:eks:cluster-name'),
                    tags.get('elbv2.k8s.aws/cluster'), tags.get('KubernetesCluster')}):
                found.append(f'cluster tag {cluster}')
            if text and cluster in text:
                found.append(f'mentions cluster {cluster}')
        if vpc and vpc in self.vpcs:
            found.append(f'in {vpc}')
        if any(name and name.startswith(self.prefix) for name in names) or \
                any(name and self.prefix in name for name in contains):
            found.append(f'name {self.prefix}')
        return found


def finding(kind, ident, state, reasons):
    return {'type': kind, 'id': ident, 'state': state, 'reason': '; '.join(reasons)}


def check_eks(call, ctx):
    found = []
    for name in call('eks', 'list-clusters').get('clusters', []):
        reasons = ctx.reasons(ids=[name], names=[name]) or ['billable EKS cluster']
        found.append(finding('eks-cluster', name, 'present', reasons))
    return found


def check_instances(call, ctx):
    found = []
    data = call('ec2', 'describe-instances', '--filters', f'Name=instance-state-name,Values={LIVE_INSTANCE_STATES}')
    for reservation in data.get('Reservations', []):
        for item in reservation.get('Instances', []):
            reasons = ctx.reasons(ids=[item.get('InstanceId')], resource=item, vpc=item.get('VpcId'))
            if reasons:
                found.append(finding('ec2-instance', item['InstanceId'], item.get('State', {}).get('Name'), reasons))
    return found


def check_rds(call, ctx):
    found = []
    for item in call('rds', 'describe-db-instances').get('DBInstances', []):
        name = item.get('DBInstanceIdentifier')
        reasons = ctx.reasons(ids=[name, item.get('DBInstanceArn'), item.get('DbiResourceId')], resource=item,
                              vpc=(item.get('DBSubnetGroup') or {}).get('VpcId'), names=[name])
        if reasons:
            found.append(finding('rds-instance', name, item.get('DBInstanceStatus'), reasons))
    for kind in ('manual', 'automated'):
        data = call('rds', 'describe-db-snapshots', '--snapshot-type', kind)
        for item in data.get('DBSnapshots', []):
            name, instance = item.get('DBSnapshotIdentifier'), item.get('DBInstanceIdentifier')
            reasons = ctx.reasons(ids=[name, instance, item.get('DBSnapshotArn'), item.get('DbiResourceId')],
                                  resource=item, vpc=item.get('VpcId'), names=[name, instance])
            if reasons:
                found.append(finding(f'rds-snapshot-{kind}', name, item.get('Status'), reasons))
    for item in call('rds', 'describe-db-instance-automated-backups').get('DBInstanceAutomatedBackups', []):
        instance = item.get('DBInstanceIdentifier')
        reasons = ctx.reasons(ids=[instance, item.get('DbiResourceId'), item.get('DBInstanceAutomatedBackupsArn')],
                              names=[instance])
        if reasons:
            found.append(finding('rds-retained-backup', item.get('DbiResourceId') or instance,
                                 item.get('Status'), reasons))
    return found


def check_volumes(call, ctx):
    found = []
    for item in call('ec2', 'describe-volumes').get('Volumes', []):
        attached = [attachment.get('InstanceId') for attachment in item.get('Attachments', [])]
        reasons = ctx.reasons(ids=[item.get('VolumeId'), *attached], resource=item)
        if item.get('State') == 'available':
            reasons.append('unattached')
        if reasons:
            found.append(finding('ebs-volume', item['VolumeId'], item.get('State'), reasons))
    for item in call('ec2', 'describe-snapshots', '--owner-ids', 'self').get('Snapshots', []):
        reasons = ctx.reasons(ids=[item.get('SnapshotId'), item.get('VolumeId')], resource=item,
                              text=item.get('Description', ''))
        if reasons:
            found.append(finding('ebs-snapshot', item['SnapshotId'], item.get('State'), reasons))
    return found


def check_network(call, ctx):
    found = []
    for item in call('ec2', 'describe-network-interfaces').get('NetworkInterfaces', []):
        groups = [group.get('GroupId') for group in item.get('Groups', [])]
        reasons = ctx.reasons(ids=[item.get('NetworkInterfaceId'), *groups], resource=item, vpc=item.get('VpcId'),
                              text=item.get('Description', ''))
        if reasons:
            found.append(finding('network-interface', item['NetworkInterfaceId'], item.get('Status'), reasons))
    for item in call('ec2', 'describe-nat-gateways').get('NatGateways', []):
        if item.get('State') == 'deleted':
            continue
        reasons = ctx.reasons(ids=[item.get('NatGatewayId')], resource=item, vpc=item.get('VpcId'))
        found.append(finding('nat-gateway', item['NatGatewayId'], item.get('State'),
                             reasons or ['billable NAT gateway']))
    for item in call('ec2', 'describe-addresses').get('Addresses', []):
        reasons = ctx.reasons(ids=[item.get('AllocationId'), item.get('NetworkInterfaceId')], resource=item)
        associated = bool(item.get('AssociationId'))
        if not associated:
            reasons.append('unassociated')
        if reasons:
            found.append(finding('elastic-ip', item.get('AllocationId') or item.get('PublicIp'),
                                 'associated' if associated else 'unassociated', reasons))
    return found


def elb_tags(call, arns):
    tags = {}
    for start in range(0, len(arns), 20):
        data = call('elbv2', 'describe-tags', '--resource-arns', *arns[start:start + 20])
        for item in data.get('TagDescriptions', []):
            tags[item.get('ResourceArn')] = item
    return tags


def check_load_balancers(call, ctx):
    found = []
    balancers = call('elbv2', 'describe-load-balancers').get('LoadBalancers', [])
    groups = call('elbv2', 'describe-target-groups').get('TargetGroups', [])
    tags = elb_tags(call, [item['LoadBalancerArn'] for item in balancers] + [item['TargetGroupArn'] for item in groups])
    for item in balancers:
        arn, name = item['LoadBalancerArn'], item.get('LoadBalancerName')
        reasons = ctx.reasons(ids=[arn, name], resource=tags.get(arn), vpc=item.get('VpcId'), names=[name])
        if reasons:
            found.append(finding(f'load-balancer-{item.get("Type", "unknown")}', arn,
                                 (item.get('State') or {}).get('Code'), reasons))
    for item in groups:
        arn, name = item['TargetGroupArn'], item.get('TargetGroupName')
        reasons = ctx.reasons(ids=[arn, name, *item.get('LoadBalancerArns', [])], resource=tags.get(arn),
                              vpc=item.get('VpcId'), names=[name])
        if reasons:
            found.append(finding('target-group', arn, 'present', reasons))
    return found


def name_tag(item):
    return tag_map(item).get('Name')


def check_vpc(call, ctx):
    found = []
    for item in call('ec2', 'describe-security-groups').get('SecurityGroups', []):
        reasons = ctx.reasons(ids=[item.get('GroupId')], resource=item, vpc=item.get('VpcId'),
                              names=[item.get('GroupName')])
        if reasons:
            found.append(finding('security-group', item['GroupId'], 'present', reasons))
    for item in call('ec2', 'describe-subnets').get('Subnets', []):
        reasons = ctx.reasons(ids=[item.get('SubnetId')], resource=item, vpc=item.get('VpcId'), names=[name_tag(item)])
        if reasons:
            found.append(finding('subnet', item['SubnetId'], item.get('State'), reasons))
    for item in call('ec2', 'describe-vpcs').get('Vpcs', []):
        reasons = ctx.reasons(ids=[item.get('VpcId')], resource=item, names=[name_tag(item)])
        if reasons:
            found.append(finding('vpc', item['VpcId'], item.get('State'), reasons))
    return found


def check_logs(call, ctx):
    found = []
    for item in call('logs', 'describe-log-groups').get('logGroups', []):
        name = item.get('logGroupName', '')
        reasons = ctx.reasons(ids=[name, item.get('arn')], contains=[name])
        reasons += [f'cluster log group {cluster}' for cluster in ctx.clusters
                    if name.startswith(f'/aws/eks/{cluster}/')]
        if reasons:
            found.append(finding('log-group', name, 'present', sorted(set(reasons))))
    return found


def check_secrets(call, ctx):
    found, listed = [], set()
    for item in call('secretsmanager', 'list-secrets').get('SecretList', []):
        arn, name = item.get('ARN'), item.get('Name')
        listed.add(arn)
        owner = tag_map(item).get('aws:rds:primaryDBInstanceArn')
        reasons = ctx.reasons(ids=[arn, name, owner], resource=item, contains=[name])
        if reasons:
            found.append(finding('secret', arn, 'present', reasons))
    for arn in ctx.secret_arns:
        if arn in listed:
            continue
        try:
            item = call('secretsmanager', 'describe-secret', '--secret-id', arn)
        except QueryError as error:
            if 'ResourceNotFoundException' in str(error):
                continue
            raise
        if not item.get('DeletedDate'):
            found.append(finding('secret', arn, 'present', ['inventory']))
    return found


def check_buckets(call, ctx):
    found, retained = [], []
    for item in call('s3api', 'list-buckets').get('Buckets', []):
        name = item.get('Name')
        if name == ctx.state_bucket:
            retained.append(finding('s3-bucket', name, 'present', ['state bucket, kept on purpose']))
            continue
        reasons = ctx.reasons(ids=[name], names=[name])
        if reasons:
            found.append(finding('s3-bucket', name, 'present', reasons))
    return found, retained


CHECKS = (('eks', check_eks), ('ec2-instances', check_instances), ('rds', check_rds), ('ebs', check_volumes),
          ('network', check_network), ('load-balancers', check_load_balancers), ('vpc', check_vpc),
          ('log-groups', check_logs), ('secrets', check_secrets))


def check_once(call, ctx):
    result = {'leftovers': [], 'retained': [], 'unchecked': []}
    for name, check in CHECKS + (('s3', check_buckets),):
        try:
            found = check(call, ctx)
        except QueryError as error:
            result['unchecked'].append({'check': name, 'error': str(error)})
            continue
        if name == 's3':
            found, retained = found
            result['retained'] += retained
        result['leftovers'] += found
    result['exit_code'] = 2 if result['unchecked'] else 1 if result['leftovers'] else 0
    return result


def print_result(result):
    rows = [(item['type'], item['id'], str(item['state']), item['reason']) for item in result['leftovers']]
    if rows:
        header = ('TYPE', 'ID', 'STATE', 'REASON')
        widths = [max(len(row[i]) for row in rows + [header]) for i in range(3)]
        for row in [header, *rows]:
            print('  '.join(cell.ljust(width) for cell, width in zip(row[:3], widths)) + '  ' + row[3])
    for item in result['retained']:
        print(f"retained: {item['type']} {item['id']} ({item['reason']})")
    for item in result['unchecked']:
        print(f"unchecked: {item['check']}: {item['error']}")
    if result['exit_code'] == 0:
        print('no billable leftovers found')
    elif result['unchecked']:
        print(f"{len(result['unchecked'])} checks failed; their resources are unknown, not absent")


def load_inventory(path):
    if path is None:
        return None
    try:
        inventory = json.loads(Path(path).read_text(encoding='utf-8'))
    except (OSError, ValueError) as error:
        raise OperatorError(f'cannot read inventory {path}: {type(error).__name__}') from error
    if not isinstance(inventory, dict) or inventory.get('kind') != 'gateflow-teardown-inventory':
        raise OperatorError(f'{path} is not a file written by `leftovers.py inventory`')
    return inventory


def run_check(args, runner, sleep, clock):
    ctx = Context(args, load_inventory(args.inventory))
    if args.inventory is None:
        print('WARNING no --inventory: resources without tags or the name prefix may be missed', file=sys.stderr)
    call = aws_caller(runner, args.region)
    deadline = clock() + args.wait
    while True:
        result = check_once(call, ctx)
        remaining = deadline - clock()
        if result['exit_code'] == 0 or remaining <= 0:
            break
        print(f"{len(result['leftovers'])} leftovers, {len(result['unchecked'])} unchecked; "
              f'checking again in {min(args.interval, remaining):.0f}s', file=sys.stderr, flush=True)
        sleep(min(args.interval, remaining))
    if args.json:
        print(json.dumps(result, indent=2))
    else:
        print_result(result)
    return result['exit_code']


def collect(call, outputs, inventory, unchecked, name_prefix):
    """Fills inventory from AWS; each failed query is recorded in unchecked."""
    cluster, vpc = outputs.get('cluster_name') or name_prefix, outputs.get('vpc_id')
    database = (outputs.get('database_address') or '').split('.')[0] or name_prefix
    by_vpc = ('--filters', f'Name=vpc-id,Values={vpc}')

    def query(label, function):
        try:
            function()
        except (QueryError, KeyError, TypeError, AttributeError) as error:
            unchecked.append({'query': label, 'error': str(error)})

    def add(key, *values):
        inventory[key].update(value for value in values if value)

    def eks():
        item = call('eks', 'describe-cluster', '--name', cluster)['cluster']
        add('cluster_arns', item.get('arn'))
        add('security_group_ids', (item.get('resourcesVpcConfig') or {}).get('clusterSecurityGroupId'))

    def instances():
        for reservation in call('ec2', 'describe-instances', *by_vpc).get('Reservations', []):
            for item in reservation.get('Instances', []):
                add('instance_ids', item.get('InstanceId'))
                add('volume_ids', *[(m.get('Ebs') or {}).get('VolumeId') for m in item.get('BlockDeviceMappings', [])])
                add('network_interface_ids', *[n.get('NetworkInterfaceId') for n in item.get('NetworkInterfaces', [])])

    def volumes():
        tagged = call('ec2', 'describe-volumes', '--filters', f'Name=tag-key,Values=kubernetes.io/cluster/{cluster}')
        add('volume_ids', *[item.get('VolumeId') for item in tagged.get('Volumes', [])])

    def network():
        add('security_group_ids', *[item.get('GroupId') for item in
                                    call('ec2', 'describe-security-groups', *by_vpc).get('SecurityGroups', [])])
        add('network_interface_ids', *[item.get('NetworkInterfaceId') for item in
                                       call('ec2', 'describe-network-interfaces', *by_vpc).get('NetworkInterfaces', [])])
        for item in call('ec2', 'describe-nat-gateways', '--filter', f'Name=vpc-id,Values={vpc}').get('NatGateways', []):
            add('nat_gateway_ids', item.get('NatGatewayId'))
            add('allocation_ids', *[address.get('AllocationId') for address in item.get('NatGatewayAddresses', [])])

    def balancers():
        add('load_balancer_arns', *[item['LoadBalancerArn'] for item in
                                    call('elbv2', 'describe-load-balancers').get('LoadBalancers', [])
                                    if item.get('VpcId') == vpc])
        add('target_group_arns', *[item['TargetGroupArn'] for item in
                                   call('elbv2', 'describe-target-groups').get('TargetGroups', [])
                                   if item.get('VpcId') == vpc])

    def logs():
        for prefix in (f'/aws/eks/{cluster}/', f'/aws/rds/instance/{database}/'):
            data = call('logs', 'describe-log-groups', '--log-group-name-prefix', prefix)
            add('log_group_names', *[item.get('logGroupName') for item in data.get('logGroups', [])])

    def rds():
        for item in call('rds', 'describe-db-instances', '--db-instance-identifier', database).get('DBInstances', []):
            add('db_instance_arns', item.get('DBInstanceArn'))
            add('db_resource_ids', item.get('DbiResourceId'))
            add('secret_arns', (item.get('MasterUserSecret') or {}).get('SecretArn'))

    query('eks describe-cluster', eks)
    if vpc:
        query('ec2 describe-instances', instances)
        query('ec2 security groups, network interfaces and NAT gateways', network)
        query('elbv2 load balancers and target groups', balancers)
    query('ec2 describe-volumes', volumes)
    query('logs describe-log-groups', logs)
    query('rds describe-db-instances', rds)


def run_inventory(args, runner, now):
    require_ignored(args.out, runner, 'inventory')
    outputs = terraform_outputs(args.terraform_output, ('vpc_id', 'cluster_name'))
    inventory = {key: set() for key in INVENTORY_KEYS}
    cluster = outputs['cluster_name']
    inventory['cluster_names'].add(cluster)
    inventory['vpc_ids'].add(outputs['vpc_id'])
    inventory['subnet_ids'].update(outputs.get('public_subnet_ids') or [])
    inventory['subnet_ids'].update(outputs.get('private_subnet_ids') or [])
    inventory['security_group_ids'].update(filter(None, [outputs.get('service_security_group_id'),
                                                         outputs.get('generator_security_group_id'),
                                                         outputs.get('model_host_security_group_id')]))
    # The optional GPU model host (gpu_model_enabled); its outputs are null when it is off.
    inventory['instance_ids'].update(filter(None, [outputs.get('model_host_instance_id')]))
    inventory['role_arns'].update(filter(None, [outputs.get('controller_role_arn'), outputs.get('model_host_role_arn')]))
    inventory['secret_arns'].update(filter(None, [outputs.get('master_secret_arn')]))
    address = outputs.get('database_address')
    if address:
        inventory['db_instance_identifiers'].add(address.split('.')[0])
    unchecked = []
    collect(aws_caller(runner, args.region), outputs, inventory, unchecked, args.name_prefix)
    document = {'kind': 'gateflow-teardown-inventory', 'created': now.strftime('%Y-%m-%dT%H:%M:%SZ'),
                'region': args.region, 'name_prefix': args.name_prefix,
                **{key: sorted(values) for key, values in inventory.items()}, 'unchecked': unchecked}
    write_private(args.out, json.dumps(document, indent=2) + '\n')
    counts = ', '.join(f'{key} {len(document[key])}' for key in INVENTORY_KEYS if document[key])
    print(f'inventory written to {args.out}: {counts}')
    for item in unchecked:
        print(f"unchecked: {item['query']}: {item['error']}")
    return 2 if unchecked else 0


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    commands = parser.add_subparsers(dest='command', required=True)
    for name in ('inventory', 'check'):
        sub = commands.add_parser(name)
        sub.add_argument('--region', default=REGION)
        sub.add_argument('--name-prefix', default=NAME_PREFIX)
        sub.add_argument('--project', default=PROJECT)
        sub.add_argument('--environment', default=ENVIRONMENT)
    inventory, check = commands.choices['inventory'], commands.choices['check']
    inventory.add_argument('--terraform-output', type=Path, required=True, metavar='JSON',
                           help='file holding the output of `terraform output -json`')
    inventory.add_argument('--out', type=Path, default=DEFAULT_INVENTORY,
                           help='inventory file; must be ignored by git (default: .local/cloud-day/teardown-inventory.json)')
    check.add_argument('--inventory', type=Path, help='file written by the inventory command before destroy')
    check.add_argument('--state-bucket', help='Terraform state bucket, listed only as retained')
    check.add_argument('--json', action='store_true', help='print the machine-readable result')
    check.add_argument('--wait', type=int, default=0, metavar='SECONDS',
                       help='keep checking until nothing is left or SECONDS have passed (default: check once)')
    check.add_argument('--interval', type=int, default=30, metavar='SECONDS', help='seconds between checks (default: 30)')
    args = parser.parse_args(argv)
    if args.command == 'check' and (args.wait < 0 or args.interval < 1):
        parser.error('--wait must be >= 0 and --interval >= 1')
    return args


def main(argv=None, runner=subprocess.run, sleep=time.sleep, clock=time.monotonic, now=None):
    args = parse_args(argv)
    try:
        if args.command == 'inventory':
            return run_inventory(args, runner, now or datetime.now(timezone.utc))
        return run_check(args, runner, sleep, clock)
    except OperatorError as error:
        print(f'FAIL {error}', file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
