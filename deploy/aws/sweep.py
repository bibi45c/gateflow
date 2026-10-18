"""Second, independent check for billable leftovers: every region, by resource type.

Operator-run (bibi45c only), read-only, after `leftovers.py check`. Unlike leftovers.py
it needs no inventory and no tags. In each region (--regions, or every region that
`aws ec2 describe-regions` lists for the account) it lists every EKS cluster; EC2
instance that is pending, running, stopping or stopped; NAT gateway that is pending,
available or deleting; Elastic IP; ELBv2 load balancer; RDS instance; manual RDS
snapshot; EBS volume; and non-default VPC, whatever project it belongs to. S3 is not
checked: the Terraform state bucket is kept on purpose.

Exit codes: 0 nothing found in any region, 1 something found, 2 a query failed (its
resources are unknown, not absent).
"""
import argparse
import json
import os
import subprocess
import sys

from common import REGION

LIVE_INSTANCES = 'Name=instance-state-name,Values=pending,running,stopping,stopped'
LIVE_NAT = 'Name=state,Values=pending,available,deleting'


class QueryError(Exception):
    pass


def aws_caller(runner):
    env = os.environ | {'AWS_PAGER': ''}

    def call(region, *args):
        command = ['aws', *args, '--output', 'json', '--region', region]
        try:
            result = runner(command, capture_output=True, text=True, encoding='utf-8', errors='replace', env=env)
        except OSError as error:
            raise QueryError(f'{" ".join(args[:2])} {region}: aws could not start: {error.strerror}') from error
        if result.returncode:
            raise QueryError(f'{" ".join(args[:2])} {region}: {(result.stderr or "").strip()[:160]}')
        try:
            return json.loads(result.stdout or '{}')
        except ValueError as error:
            raise QueryError(f'{" ".join(args[:2])} {region}: the answer is not JSON') from error
    return call


def checks(call, region):
    """Resource type -> function listing the identifiers of that type in region."""
    return {
        'eks': lambda: call(region, 'eks', 'list-clusters')['clusters'],
        'ec2': lambda: [item['InstanceId'] for reservation in
                        call(region, 'ec2', 'describe-instances', '--filters', LIVE_INSTANCES)['Reservations']
                        for item in reservation['Instances']],
        'nat': lambda: [item['NatGatewayId'] for item in
                        call(region, 'ec2', 'describe-nat-gateways', '--filter', LIVE_NAT)['NatGateways']],
        'eip': lambda: [item.get('AllocationId') for item in call(region, 'ec2', 'describe-addresses')['Addresses']],
        'elb': lambda: [item['LoadBalancerName'] for item in
                        call(region, 'elbv2', 'describe-load-balancers')['LoadBalancers']],
        'rds': lambda: [item['DBInstanceIdentifier'] for item in
                        call(region, 'rds', 'describe-db-instances')['DBInstances']],
        'rds-snap': lambda: [item['DBSnapshotIdentifier'] for item in
                             call(region, 'rds', 'describe-db-snapshots', '--snapshot-type', 'manual')['DBSnapshots']],
        'ebs': lambda: [item['VolumeId'] for item in call(region, 'ec2', 'describe-volumes')['Volumes']],
        'vpc': lambda: [item['VpcId'] for item in
                        call(region, 'ec2', 'describe-vpcs', '--filters', 'Name=is-default,Values=false')['Vpcs']],
    }


def sweep(call, regions):
    found, failed = [], []
    for region in regions:
        for name, function in checks(call, region).items():
            try:
                items = function()
            except QueryError as error:
                failed.append(str(error))
                continue
            except (KeyError, TypeError) as error:
                failed.append(f'{name} {region}: unexpected answer ({type(error).__name__}: {error})')
                continue
            if items:
                found.append(f'{region} {name}: {len(items)} ({", ".join(map(str, items[:5]))})')
    return found, failed


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--regions', nargs='+', metavar='REGION', help='regions to check (default: every enabled region)')
    parser.add_argument('--region', default=REGION, help=f'region that lists the enabled regions (default: {REGION})')
    return parser.parse_args(argv)


def main(argv=None, runner=subprocess.run):
    args = parse_args(argv)
    call = aws_caller(runner)
    regions, failed = args.regions, []
    if not regions:
        try:
            regions = [item['RegionName'] for item in call(args.region, 'ec2', 'describe-regions')['Regions']]
        except (QueryError, KeyError, TypeError) as error:
            regions, failed = [], [f'describe-regions: {error}']
    found, more = sweep(call, regions)
    failed += more
    print(f'regions checked: {len(regions)}')
    for line in found:
        print('FOUND', line)
    for line in failed:
        print('QUERY FAILED', line)
    print('result:', 'query failures' if failed else ('billable resources found' if found
                                                       else 'nothing billable found in any region'))
    return 2 if failed else 1 if found else 0


if __name__ == '__main__':
    sys.exit(main())
