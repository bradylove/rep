package lrp_scheduler_test

import (
	"errors"
	"time"

	"github.com/cloudfoundry-incubator/executor/api"
	"github.com/cloudfoundry-incubator/executor/client/fake_client"
	. "github.com/cloudfoundry-incubator/rep/lrp_scheduler"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs/fake_bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry/gosteno"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Scheduler", func() {
	var logger *gosteno.Logger

	BeforeSuite(func() {
		gosteno.EnterTestMode(gosteno.LOG_DEBUG)
	})

	BeforeEach(func() {
		logger = gosteno.NewLogger("test-logger")
	})

	Context("when a game scheduler is running", func() {
		var fakeBBS *fake_bbs.FakeRepBBS
		var lrpScheduler *LrpScheduler
		var correctStack = "correct-stack"
		var fakeClient *fake_client.FakeClient

		var lrp models.TransitionalLongRunningProcess

		BeforeEach(func() {
			fakeClient = fake_client.New()
			fakeBBS = fake_bbs.NewFakeRepBBS()

			numFiles := uint64(16)
			zero := 0
			lrp = models.TransitionalLongRunningProcess{
				Guid:     "app-guid-app-version",
				Stack:    correctStack,
				MemoryMB: 128,
				DiskMB:   1024,
				Actions: []models.ExecutorAction{
					{
						Action: models.DownloadAction{
							From:     "http://droplet.url",
							To:       "/app",
							Extract:  true,
							CacheKey: "droplet-app-guid-app-version",
						},
					},
					{
						Action: models.RunAction{
							Script: "the-script",
							Env: []models.EnvironmentVariable{
								{
									Key:   "THE_KEY",
									Value: "THE_VALUE",
								},
							},
							Timeout: time.Second,
							ResourceLimits: models.ResourceLimits{
								Nofile: &numFiles,
							},
						},
					},
				},
				Log: models.LogConfig{
					Guid:       "app-guid",
					SourceName: "APP",
					Index:      &zero,
				},
				State: models.TransitionalLRPStateDesired,
			}

			lrpScheduler = New(fakeBBS, logger, correctStack, fakeClient)
		})

		AfterEach(func() {
			lrpScheduler.Stop()
		})

		BeforeEach(func() {
			readyChan := make(chan struct{})
			lrpScheduler.Run(readyChan)
			<-readyChan
		})

		Context("when a LRP is desired", func() {
			JustBeforeEach(func() {
				fakeBBS.EmitDesiredLrp(lrp)
			})

			Context("when reserving the container succeeds", func() {
				var allocateCalled chan struct{}
				var deletedContainerGuids chan string
				var containerGuid string

				BeforeEach(func() {
					allocateCalled = make(chan struct{})
					deletedContainerGuids = make(chan string, 1)

					fakeClient.WhenAllocatingContainer = func(guid string, req api.ContainerAllocationRequest) (api.Container, error) {
						defer GinkgoRecover()

						close(allocateCalled)
						Ω(fakeBBS.StartedLongRunningProcesses()).Should(HaveLen(0))
						Ω(req.MemoryMB).Should(Equal(128))
						Ω(req.DiskMB).Should(Equal(1024))

						containerGuid = guid
						Ω(guid).ShouldNot(BeZero())

						return api.Container{ExecutorGuid: "the-executor-guid", Guid: containerGuid}, nil
					}

					fakeClient.WhenDeletingContainer = func(allocationGuid string) error {
						deletedContainerGuids <- allocationGuid
						return nil
					}
				})

				Context("when starting the LRP succeeds", func() {
					Context("when initializing the container succeeds", func() {
						var initCalled chan struct{}

						BeforeEach(func() {
							initCalled = make(chan struct{})

							fakeClient.WhenInitializingContainer = func(allocationGuid string, req api.ContainerInitializationRequest) error {
								defer GinkgoRecover()

								close(initCalled)
								Ω(allocationGuid).Should(Equal(containerGuid))
								Ω(req.Log).Should(Equal(lrp.Log))
								Ω(fakeBBS.StartedLongRunningProcesses()).Should(HaveLen(1))
								Ω(fakeBBS.StartedLongRunningProcesses()[0]).Should(Equal(lrp))
								return nil
							}
						})

						Context("and the executor successfully starts running the LRP", func() {
							var runCalled chan struct{}

							BeforeEach(func() {
								runCalled = make(chan struct{})

								fakeClient.WhenRunning = func(allocationGuid string, req api.ContainerRunRequest) error {
									defer GinkgoRecover()

									close(runCalled)

									Ω(fakeBBS.StartedLongRunningProcesses()).Should(HaveLen(1))
									Ω(fakeBBS.StartedLongRunningProcesses()[0]).Should(Equal(lrp))

									Ω(allocationGuid).Should(Equal(containerGuid))
									Ω(req.Actions).Should(Equal(lrp.Actions))

									return nil
								}
							})

							It("makes all calls to the executor", func() {
								Eventually(allocateCalled).Should(BeClosed())
								Eventually(initCalled).Should(BeClosed())
								Eventually(runCalled).Should(BeClosed())
							})
						})
					})

					Context("but initializing the container fails", func() {
						BeforeEach(func() {
							fakeClient.WhenInitializingContainer = func(allocationGuid string, req api.ContainerInitializationRequest) error {
								return errors.New("Can't initialize")
							}
						})

						It("does not mark the job as started", func() {
							Eventually(fakeBBS.StartedLongRunningProcesses).Should(HaveLen(0))
						})

						It("deletes the container", func() {
							var deletedGuid string
							Eventually(deletedContainerGuids).Should(Receive(&deletedGuid))
							Ω(deletedGuid).Should(Equal(containerGuid))
						})
					})
				})

				Context("but starting the LRP fails", func() {
					BeforeEach(func() {
						fakeBBS.SetStartLrpErr(errors.New("data store went away."))
					})

					It("deletes the resource allocation on the executor", func() {
						var deletedGuid string
						Eventually(deletedContainerGuids).Should(Receive(&deletedGuid))
						Ω(deletedGuid).Should(Equal(containerGuid))
					})
				})
			})

			Context("with a mismatched stack", func() {
				BeforeEach(func() {
					mismatchedLRP := lrp
					mismatchedLRP.Stack = "some-bogus-stack"

					lrp = mismatchedLRP
				})

				It("does not try to start it", func() {
					Consistently(fakeBBS.StartedLongRunningProcesses).Should(BeEmpty())
				})
			})

			Context("when reserving the container fails", func() {
				var allocatedContainer chan struct{}

				BeforeEach(func() {
					allocatedContainer = make(chan struct{})

					fakeClient.WhenAllocatingContainer = func(guid string, req api.ContainerAllocationRequest) (api.Container, error) {
						close(allocatedContainer)
						return api.Container{}, errors.New("Something went wrong")
					}
				})

				It("makes the resource allocation request", func() {
					Eventually(allocatedContainer).Should(BeClosed())
				})

				It("does not mark the job as Started", func() {
					Eventually(fakeBBS.StartedLongRunningProcesses).Should(HaveLen(0))
				})
			})
		})
	})
})
