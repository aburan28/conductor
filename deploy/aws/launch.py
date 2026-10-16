#!/usr/bin/env python3
"""Launch Conductor on one EC2 instance, check on it, and tear it down.

  launch.py launch [--allow-cidr 203.0.113.9/32] [--region us-east-1] [--instance-type t3.small]
  launch.py allow --allow-cidr 203.0.113.9/32      (open the port later)
  launch.py status
  launch.py teardown

The instance runs Ubuntu 24.04, the distribution's Postgres on loopback, and a pinned build of
Conductor under systemd, serving HTTPS with a self-signed certificate in "enhanced" security
mode (tokens only). TCP 8443 is opened only to --allow-cidr, and stays closed without it. There is no SSH key and
no SSM role: progress and the first admin token come back through the serial console.

Everything it creates is tagged conductor-aws-demo=<name>. `teardown` removes it.

STATUS: written while the only AWS credentials available were rejected by AWS (InvalidClientTokenId),
so this has been syntax-checked and its instance-side commands exercised on Linux, but never run
against AWS.
"""
import argparse, os, re, ssl, sys, time, urllib.request
import boto3
from botocore.exceptions import ClientError

HERE = os.path.dirname(os.path.abspath(__file__))
TAG = "conductor-aws-demo"
COMMIT = "54b09047331c94297b95d934cbe9d9786538d7f9"  # default build: main when this was written; override with --commit
GO_SHA256 = {  # go1.26.9, from https://go.dev/dl/?mode=json
    "amd64": "42d158b4d8f7b61ac0a830567c940a86098fb7aac52e467a5ebec03ef5cc2f8d",
    "arm64": "4a97373d49fcacdcf3694fea368a500b00ee3e963974f3e7514132717632f052",
}
PORT = 8443


def render_userdata(arch: str, commit: str) -> str:
    text = open(os.path.join(HERE, "userdata.sh")).read()
    for key, val in {"__COMMIT__": commit, "__GO_SHA256__": GO_SHA256[arch], "__GOARCH__": arch}.items():
        text = text.replace(key, val)
    left = re.findall(r"__[A-Z0-9_]+__", text)
    if left:
        sys.exit(f"unfilled placeholders in userdata.sh: {left}")
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        sys.exit("--commit must be a full 40-character commit sha")
    if len(text.encode()) > 16000:
        sys.exit("user-data is over EC2's 16 KB limit")
    return text


def clients(region):
    return boto3.client("ec2", region_name=region), boto3.client("ssm", region_name=region)


def find_instances(ec2, name):
    r = ec2.describe_instances(Filters=[
        {"Name": f"tag:{TAG}", "Values": [name]},
        {"Name": "instance-state-name", "Values": ["pending", "running", "stopping", "stopped"]}])
    return [i for res in r["Reservations"] for i in res["Instances"]]


def open_port(ec2, sg, cidr):
    ec2.authorize_security_group_ingress(GroupId=sg, IpPermissions=[{
        "IpProtocol": "tcp", "FromPort": PORT, "ToPort": PORT,
        "IpRanges": [{"CidrIp": cidr, "Description": "Conductor HTTPS"}]}])
    print(f"opened tcp/{PORT} to {cidr}")


def allow(a):
    if not a.allow_cidr:
        sys.exit("allow needs --allow-cidr")
    ec2, _ = clients(a.region)
    groups = ec2.describe_security_groups(Filters=[{"Name": f"tag:{TAG}", "Values": [a.name]}])["SecurityGroups"]
    if not groups:
        sys.exit(f"no security group tagged {TAG}={a.name}")
    open_port(ec2, groups[0]["GroupId"], a.allow_cidr)


def launch(a):
    arch = "arm64" if a.instance_type.split(".")[0].endswith("g") else "amd64"
    ec2, ssm = clients(a.region)
    if find_instances(ec2, a.name):
        sys.exit(f"an instance tagged {TAG}={a.name} already exists; run `status` or `teardown`")
    userdata = render_userdata(arch, a.commit)

    ami = ssm.get_parameter(Name=f"/aws/service/canonical/ubuntu/server/24.04/stable/current/{arch}/hvm/ebs-gp3/ami-id")["Parameter"]["Value"]
    vpcs = ec2.describe_vpcs(Filters=[{"Name": "isDefault", "Values": ["true"]}])["Vpcs"]
    if not vpcs and not a.subnet_id:
        sys.exit("no default VPC in this region; pass --subnet-id (a public subnet) and --vpc-id")
    vpc_id = a.vpc_id or vpcs[0]["VpcId"]

    sg = ec2.create_security_group(GroupName=f"{TAG}-{a.name}-{int(time.time())}", VpcId=vpc_id,
                                   Description="Conductor: TLS port from one CIDR",
                                   TagSpecifications=[{"ResourceType": "security-group", "Tags": [{"Key": TAG, "Value": a.name}]}])["GroupId"]
    if a.allow_cidr:
        open_port(ec2, sg, a.allow_cidr)
    else:
        print("no --allow-cidr given: the port stays closed; open it later with `allow --allow-cidr X`")
    nic = {"DeviceIndex": 0, "AssociatePublicIpAddress": True, "Groups": [sg]}
    if a.subnet_id:
        nic["SubnetId"] = a.subnet_id
    try:
        inst = ec2.run_instances(
            ImageId=ami, InstanceType=a.instance_type, MinCount=1, MaxCount=1, UserData=userdata,
            NetworkInterfaces=[nic],
            BlockDeviceMappings=[{"DeviceName": "/dev/sda1", "Ebs": {"VolumeSize": 20, "VolumeType": "gp3", "Encrypted": True, "DeleteOnTermination": True}}],
            MetadataOptions={"HttpTokens": "required", "HttpEndpoint": "enabled"},
            TagSpecifications=[{"ResourceType": r, "Tags": [{"Key": TAG, "Value": a.name}, {"Key": "Name", "Value": f"conductor-{a.name}"}]}
                               for r in ("instance", "volume")])["Instances"][0]
    except ClientError:
        ec2.delete_security_group(GroupId=sg)  # do not leave a group behind
        raise
    iid = inst["InstanceId"]
    print(f"launched {iid} ({a.instance_type}, {arch}, {ami}) in {a.region}; security group {sg}")
    ec2.get_waiter("instance_running").wait(InstanceIds=[iid])
    ip = ec2.describe_instances(InstanceIds=[iid])["Reservations"][0]["Instances"][0].get("PublicIpAddress")
    print(f"running at {ip}; waiting for the bootstrap (about 5-8 minutes: it builds Conductor)")

    deadline, seen = time.time() + a.timeout, ""
    while time.time() < deadline:
        time.sleep(20)
        out = ec2.get_console_output(InstanceId=iid, Latest=True).get("Output") or ""
        out = re.sub(r"\x1b\[[0-9;]*m", "", out)
        for line in out.splitlines()[len(seen.splitlines()):]:
            if "conductor-bootstrap" in line and "CONDUCTOR_TOKEN" not in line:
                print("  " + line.strip())
        seen = out
        if "CONDUCTOR_READY" in out or "conductor-bootstrap: FAILED" in out:
            break
    url = re.search(r"CONDUCTOR_URL=(\S+)", seen)
    tok = re.search(r"CONDUCTOR_TOKEN=(cdt_\S+)", seen)
    if not (url and tok and "CONDUCTOR_READY" in seen):
        sys.exit(f"not ready: see `aws ec2 get-console-output --instance-id {iid}`; `teardown` removes everything")
    path = os.path.abspath(a.token_file)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(tok.group(1) + "\n")
    print(f"\nConductor is up: {url.group(1)}")
    print(f"admin token (valid 24h) written to {path} (mode 0600); rotate it after you sign in")
    try:
        ctx = ssl.create_default_context(); ctx.check_hostname = False; ctx.verify_mode = ssl.CERT_NONE
        code = urllib.request.urlopen(url.group(1) + "/v1/health", timeout=15, context=ctx).status
        print(f"health check from here: HTTP {code}")
    except Exception as e:
        print(f"health check from here not possible ({type(e).__name__}); the console reported health=200 from the instance itself")


def status(a):
    ec2, _ = clients(a.region)
    for i in find_instances(ec2, a.name):
        print(i["InstanceId"], i["State"]["Name"], i.get("PublicIpAddress"), i["InstanceType"])


def teardown(a):
    ec2, _ = clients(a.region)
    ids = [i["InstanceId"] for i in find_instances(ec2, a.name)]
    if ids:
        ec2.terminate_instances(InstanceIds=ids)
        ec2.get_waiter("instance_terminated").wait(InstanceIds=ids)
        print("terminated", *ids)
    groups = ec2.describe_security_groups(Filters=[{"Name": f"tag:{TAG}", "Values": [a.name]}])["SecurityGroups"]
    for g in groups:
        for _ in range(12):
            try:
                ec2.delete_security_group(GroupId=g["GroupId"]); print("deleted", g["GroupId"]); break
            except ClientError as e:
                if e.response["Error"]["Code"] != "DependencyViolation":
                    raise
                time.sleep(10)
    if not ids and not groups:
        print("nothing to remove")


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("command", choices=["launch", "allow", "status", "teardown"])
    p.add_argument("--region", default=os.environ.get("AWS_REGION") or os.environ.get("AWS_DEFAULT_REGION") or "us-east-1")
    p.add_argument("--name", default="conductor")
    p.add_argument("--allow-cidr", help="who may reach the HTTPS port, e.g. 203.0.113.9/32 (without it the port stays closed)")
    p.add_argument("--allow-world", action="store_true", help="allow 0.0.0.0/0")
    p.add_argument("--instance-type", default="t3.small")
    p.add_argument("--commit", default=COMMIT, help="full git commit of github.com/aburan28/conductor to build on the instance")
    p.add_argument("--vpc-id"); p.add_argument("--subnet-id")
    p.add_argument("--timeout", type=int, default=1200, help="seconds to wait for the bootstrap")
    p.add_argument("--token-file", default="conductor-admin-token.txt")
    a = p.parse_args()
    if a.allow_cidr == "0.0.0.0/0" and not a.allow_world:
        p.error("refusing to open the port to the whole internet; pass --allow-world to do that on purpose")
    {"launch": launch, "allow": allow, "status": status, "teardown": teardown}[a.command](a)


if __name__ == "__main__":
    main()
