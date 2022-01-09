require('../../shared/polyfill');

const { ensureSigningKey } = require('../secret');
const { spawn } = require('child_process');
const { CODE_ROOT } = require('../constants');
const path = require('path');
const cp = require('child_process');
const os = require('os');
const fs = require('fs');
const { getProjectResultsFile } = require('../store');
const {
  HTTPPanelInfo,
  HTTPConnectorInfo,
  ServerInfo,
} = require('../../shared/state');
const {
  withSavedPanels,
  translateBaselineForType,
  replaceBigInt,
  REGEXP_TESTS,
  RUNNERS,
} = require('./testutil');

ensureSigningKey();

const testPath = path.join(CODE_ROOT, 'testdata');
const baseline = JSON.parse(
  fs.readFileSync(path.join(testPath, 'userdata.json').toString())
);

const USERDATA_FILES = ['json', 'xlsx', 'csv', 'parquet', 'jsonl'];
const PORT = '9799';

let server;
beforeAll(async () => {
  // TODO: port this logic to other platforms...
  if (process.platform === 'linux') {
    try {
      cp.execSync(
        `bash -c "ps aux | grep 'http.server ${PORT}' | grep -v grep | awk '{print \\$2}' | xargs -I {} kill {}"`
      );
    } catch (e) {
      console.error(e);
    }
  }

  server = spawn('python3', ['-m', 'http.server', PORT]);
  let ready = false;
  server.on('spawn', () => {
    ready = true;
  });

  server.stdout.on('data', (data) => {
    console.log(data.toString());
  });

  server.stderr.on('data', (data) => {
    console.warn(data.toString());
  });

  return new Promise(async (resolve, reject) => {
    try {
      while (!ready) {
        console.log('still waiting');
        await new Promise((resolve) => setTimeout(resolve, 300));
      }

      resolve();
    } catch (e) {
      reject(e);
    }
  });
});

for (const subprocessName of RUNNERS) {
  if (!subprocessName?.go) {
    continue; // Otherwise not implemented
  }

  describe(
    'eval generic file via ' +
      (subprocessName ? subprocessName.go || subprocessName.node : 'memory'),
    () => {
      test('correct result', () => {
        const hp = new HTTPPanelInfo(
          '',
          new HTTPConnectorInfo('', 'http://localhost:9799/testdata/unknown')
        );

        const panels = [hp];

        return withSavedPanels(
          panels,
          (project) => {
            // Grab result
            const value = JSON.parse(
              fs
                .readFileSync(
                  getProjectResultsFile(project.projectName) + hp.id
                )
                .toString()
            );

            expect(value).toEqual('hey this is unknown');
          },
          { evalPanels: true, subprocessName }
        );
      }, 30_000);
    }
  );

  for (const userdataFileType of USERDATA_FILES) {
    // Parquet over HTTP is broken in the Node runners
    if (userdataFileType === 'parquet' && !subprocessName?.go) {
      continue;
    }

    const hp = new HTTPPanelInfo(
      '',
      new HTTPConnectorInfo(
        '',
        'http://localhost:9799/testdata/userdata.' + userdataFileType
      )
    );

    const panels = [hp];

    describe(
      'eval ' +
        userdataFileType +
        ' file via ' +
        (subprocessName ? subprocessName.go || subprocessName.node : 'memory'),
      () => {
        test('correct result', () => {
          return withSavedPanels(
            panels,
            (project) => {
              // Grab result
              const value = JSON.parse(
                fs
                  .readFileSync(
                    getProjectResultsFile(project.projectName) + hp.id
                  )
                  .toString()
              );

              const typeBaseline = translateBaselineForType(
                baseline,
                userdataFileType
              );

              // Parquet results seem to come out unsorted
              if (userdataFileType === 'parquet') {
                value.sort((r) => r.Street);
                typeBaseline.sort((r) => r.Street);
              }
              expect(replaceBigInt(value)).toStrictEqual(
                replaceBigInt(typeBaseline)
              );
            },
            { evalPanels: true, subprocessName }
          );
        }, 30_000);
      }
    );
  }

  for (const t of REGEXP_TESTS) {
    const hp = new HTTPPanelInfo(
      '',
      new HTTPConnectorInfo(
        '',
        'http://localhost:9799/testdata/logs/' + t.filename
      )
    );
    hp.http.http.contentTypeInfo = t.contentTypeInfo;

    const panels = [hp];

    describe(
      'read ' +
        t.filename +
        ' file from disk via ' +
        (subprocessName ? subprocessName.go || subprocessName.node : 'memory'),
      () => {
        test('correct result', () => {
          return withSavedPanels(
            panels,
            (project) => {
              // Grab result
              const value = JSON.parse(
                fs
                  .readFileSync(
                    getProjectResultsFile(project.projectName) + hp.id
                  )
                  .toString()
              );

              expect(value).toStrictEqual(t.expected);
            },
            { evalPanels: true, subprocessName }
          );
        }, 10_000);
      }
    );
  }

  if (process.platform === 'linux') {
    describe(
      'eval file over server via ' +
        (subprocessName ? subprocessName.go || subprocessName.node : 'memory'),
      () => {
        test('correct result', () => {
          const server = new ServerInfo({
            address: 'localhost',
            type: 'private-key',
          });
          const hp = new HTTPPanelInfo(
            '',
            new HTTPConnectorInfo('', 'http://localhost:9799/testdata/unknown')
          );
          hp.serverId = server.id;

          const servers = [server];
          const panels = [hp];

          return withSavedPanels(
            panels,
            (project) => {
              // Grab result
              const value = JSON.parse(
                fs
                  .readFileSync(
                    getProjectResultsFile(project.projectName) + hp.id
                  )
                  .toString()
              );

              expect(value).toEqual('hey this is unknown');
            },
            { evalPanels: true, subprocessName, servers }
          );
        }, 30_000);
      }
    );
  }
}

afterAll(() => {
  process.kill(server.pid);
});