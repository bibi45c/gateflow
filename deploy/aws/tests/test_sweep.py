import contextlib
import io
import json
import unittest

from loader import completed, load

sweep = load('sweep')

EMPTY = {
    ('eks', 'list-clusters'): {'clusters': []},
    ('ec2', 'describe-instances'): {'Reservations': []},
    ('ec2', 'describe-nat-gateways'): {'NatGateways': []},
    ('ec2', 'describe-addresses'): {'Addresses': []},
    ('elbv2', 'describe-load-balancers'): {'LoadBalancers': []},
    ('rds', 'describe-db-instances'): {'DBInstances': []},
    ('rds', 'describe-db-snapshots'): {'DBSnapshots': []},
    ('ec2', 'describe-volumes'): {'Volumes': []},
    ('ec2', 'describe-vpcs'): {'Vpcs': []},
}


class FakeAws:
    """Answers per (region, service, operation); a missing region answers as empty."""

    def __init__(self, regions=('ap-southeast-1', 'us-east-1'), answers=None, fail=()):
        self.regions, self.answers, self.fail = regions, answers or {}, set(fail)
        self.commands = []

    def __call__(self, command, **kwargs):
        self.commands.append(command)
        assert command[0] == 'aws' and kwargs['env']['AWS_PAGER'] == ''
        region = command[command.index('--region') + 1]
        key = (command[1], command[2])
        if (region, *key) in self.fail or key in self.fail:
            return completed(command, 255, stderr='An error occurred (UnauthorizedOperation)')
        if key == ('ec2', 'describe-regions'):
            return completed(command, 0, json.dumps({'Regions': [{'RegionName': name} for name in self.regions]}))
        return completed(command, 0, json.dumps(self.answers.get((region, *key), EMPTY[key])))


class SweepTests(unittest.TestCase):
    def main(self, runner, *argv):
        stdout = io.StringIO()
        with contextlib.redirect_stdout(stdout):
            code = sweep.main(list(argv), runner=runner)
        self.stdout = stdout.getvalue()
        return code

    def test_clean_account_exits_0(self):
        runner = FakeAws()
        self.assertEqual(self.main(runner), 0)
        self.assertIn('regions checked: 2', self.stdout)
        self.assertIn('result: nothing billable found in any region', self.stdout)
        self.assertEqual(len(runner.commands), 1 + 2 * len(EMPTY))
        live = [c for c in runner.commands if c[1:3] == ['ec2', 'describe-instances']][0]
        self.assertIn(sweep.LIVE_INSTANCES, live)

    def test_any_billable_resource_exits_1_with_its_region_and_type(self):
        runner = FakeAws(answers={
            ('us-east-1', 'ec2', 'describe-volumes'): {'Volumes': [{'VolumeId': 'vol-0aaaaaaaaaaaaaaa1'}]},
            ('ap-southeast-1', 'ec2', 'describe-vpcs'): {'Vpcs': [{'VpcId': 'vpc-0123456789abcdef0'}]},
            ('ap-southeast-1', 'ec2', 'describe-instances'): {'Reservations': [
                {'Instances': [{'InstanceId': 'i-0aaaaaaaaaaaaaaa1'}, {'InstanceId': 'i-0aaaaaaaaaaaaaaa2'}]}]}})
        self.assertEqual(self.main(runner), 1)
        self.assertIn('FOUND us-east-1 ebs: 1 (vol-0aaaaaaaaaaaaaaa1)', self.stdout)
        self.assertIn('FOUND ap-southeast-1 vpc: 1 (vpc-0123456789abcdef0)', self.stdout)
        self.assertIn('FOUND ap-southeast-1 ec2: 2 (i-0aaaaaaaaaaaaaaa1, i-0aaaaaaaaaaaaaaa2)', self.stdout)
        self.assertIn('result: billable resources found', self.stdout)

    def test_a_failed_query_exits_2_even_when_something_is_found(self):
        runner = FakeAws(answers={('us-east-1', 'eks', 'list-clusters'): {'clusters': ['gateflow-s3']}},
                         fail=[('ap-southeast-1', 'rds', 'describe-db-instances')])
        self.assertEqual(self.main(runner), 2)
        self.assertIn('QUERY FAILED rds describe-db-instances ap-southeast-1', self.stdout)
        self.assertIn('FOUND us-east-1 eks: 1', self.stdout)
        self.assertIn('result: query failures', self.stdout)

    def test_region_listing_failure_exits_2_not_clean(self):
        self.assertEqual(self.main(FakeAws(fail=[('ec2', 'describe-regions')])), 2)
        self.assertIn('regions checked: 0', self.stdout)
        self.assertIn('QUERY FAILED describe-regions', self.stdout)

    def test_unexpected_answer_is_a_query_failure(self):
        runner = FakeAws(answers={('us-east-1', 'eks', 'list-clusters'): {'unexpected': []}})
        self.assertEqual(self.main(runner), 2)

    def test_regions_option_skips_the_listing(self):
        runner = FakeAws()
        self.assertEqual(self.main(runner, '--regions', 'eu-west-1'), 0)
        self.assertNotIn(['ec2', 'describe-regions'], [c[1:3] for c in runner.commands])
        self.assertEqual({c[c.index('--region') + 1] for c in runner.commands}, {'eu-west-1'})


if __name__ == '__main__':
    unittest.main()
