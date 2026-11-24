// #1663: on a borrowed VkDevice PyroWave must never call vkDeviceWaitIdle (other users submit on
// its queues without PyroWave's lock). Creates a device on lavapipe, lends it to PyroWave, submits
// decode work, destroys everything at once, and counts vkDeviceWaitIdle calls from the library.
// Run by check.sh; validation errors (resources freed while in flight) are read from its output.
#include <pyrowave/pyrowave.h>
#include <cstdio>
#include <cstring>
#include <vector>

#define CHECK(x) do { if (!(x)) { fprintf(stderr, "FAIL: %s (line %d)\n", #x, __LINE__); return 2; } } while (0)

static int wait_idle_calls, lock_depth, lock_calls;
static PFN_vkGetDeviceProcAddr real_gdpa;

static VKAPI_ATTR VkResult VKAPI_CALL counting_wait_idle(VkDevice device)
{
	wait_idle_calls++;
	return ((PFN_vkDeviceWaitIdle)real_gdpa(device, "vkDeviceWaitIdle"))(device);
}

static VKAPI_ATTR PFN_vkVoidFunction VKAPI_CALL hooked_gdpa(VkDevice device, const char *name)
{
	if (!strcmp(name, "vkDeviceWaitIdle"))
		return (PFN_vkVoidFunction)counting_wait_idle;
	return real_gdpa(device, name);
}

static VKAPI_ATTR PFN_vkVoidFunction VKAPI_CALL hooked_gipa(VkInstance instance, const char *name)
{
	if (!strcmp(name, "vkGetDeviceProcAddr"))
		return (PFN_vkVoidFunction)hooked_gdpa;
	if (!strcmp(name, "vkDeviceWaitIdle"))
		return (PFN_vkVoidFunction)counting_wait_idle;
	return vkGetInstanceProcAddr(instance, name);
}

int main()
{
	VkApplicationInfo app = { VK_STRUCTURE_TYPE_APPLICATION_INFO };
	app.apiVersion = VK_API_VERSION_1_3;
	VkInstanceCreateInfo ici = { VK_STRUCTURE_TYPE_INSTANCE_CREATE_INFO };
	ici.pApplicationInfo = &app;
	VkInstance instance;
	CHECK(vkCreateInstance(&ici, nullptr, &instance) == VK_SUCCESS);

	uint32_t count = 1;
	VkPhysicalDevice gpu;
	vkEnumeratePhysicalDevices(instance, &count, &gpu);
	CHECK(count == 1);

	// Enable everything the device supports, as GStreamer does for the session's device.
	VkPhysicalDeviceVulkan13Features f13 = { VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_3_FEATURES };
	VkPhysicalDeviceVulkan12Features f12 = { VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_2_FEATURES, &f13 };
	VkPhysicalDeviceVulkan11Features f11 = { VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_1_FEATURES, &f12 };
	VkPhysicalDeviceFeatures2 f2 = { VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_FEATURES_2, &f11 };
	vkGetPhysicalDeviceFeatures2(gpu, &f2);

	float priority = 1.0f;
	VkDeviceQueueCreateInfo qci = { VK_STRUCTURE_TYPE_DEVICE_QUEUE_CREATE_INFO };
	qci.queueFamilyIndex = 0; // lavapipe: one graphics+compute family
	qci.queueCount = 1;
	qci.pQueuePriorities = &priority;
	VkDeviceCreateInfo dci = { VK_STRUCTURE_TYPE_DEVICE_CREATE_INFO };
	dci.pNext = &f2;
	dci.queueCreateInfoCount = 1;
	dci.pQueueCreateInfos = &qci;
	VkDevice device;
	CHECK(vkCreateDevice(gpu, &dci, nullptr, &device) == VK_SUCCESS);
	real_gdpa = (PFN_vkGetDeviceProcAddr)vkGetInstanceProcAddr(instance, "vkGetDeviceProcAddr");

	pyrowave_device_create_queue_info queue = {};
	vkGetDeviceQueue(device, 0, 0, &queue.queue);

	pyrowave_device_create_info info = {};
	info.GetInstanceProcAddr = hooked_gipa;
	info.instance = instance;
	info.physical_device = gpu;
	info.device = device;
	info.instance_create_info = &ici;
	info.device_create_info = &dci;
	info.queue_info = &queue;
	info.queue_info_count = 1;
	info.queue_lock_callback = [](void *) { lock_depth++; lock_calls++; };
	info.queue_unlock_callback = [](void *) { lock_depth--; };

	pyrowave_device pyro;
	CHECK(pyrowave_create_device(&info, &pyro) == PYROWAVE_SUCCESS);

	// lavapipe cannot run the encoder (forced subgroup sizes), so the decoder makes Granite's own
	// GPU work: decode_cpu_buffer_async submits and returns without waiting.
	const int w = 64, h = 64;
	pyrowave_decoder_create_info dinfo = {};
	dinfo.device = pyro;
	dinfo.width = w;
	dinfo.height = h;
	dinfo.chroma = PYROWAVE_CHROMA_SUBSAMPLING_420;
	pyrowave_decoder decoder;
	CHECK(pyrowave_decoder_create(&dinfo, &decoder) == PYROWAVE_SUCCESS);

	std::vector<uint8_t> planes[3] = { std::vector<uint8_t>(w * h), std::vector<uint8_t>(w * h / 4),
	                                   std::vector<uint8_t>(w * h / 4) };
	pyrowave_cpu_buffer buf = {};
	for (int i = 0; i < 3; i++)
	{
		buf.data[i] = planes[i].data();
		buf.row_stride_in_bytes[i] = i ? w / 2 : w;
		buf.plane_size_in_bytes[i] = planes[i].size();
	}
	buf.width = w;
	buf.height = h;
	buf.format = PYROWAVE_CPU_BUFFER_FORMAT_YUV420P;
	CHECK(pyrowave_decoder_decode_cpu_buffer_async(decoder, &buf, 0) == PYROWAVE_SUCCESS);

	// Tear down with that work in flight: Granite must wait for it, not for the device.
	pyrowave_decoder_destroy(decoder);
	pyrowave_device_destroy(pyro);

	CHECK(lock_depth == 0);
	printf("borrowed device: vkDeviceWaitIdle calls=%d, queue lock calls=%d\n", wait_idle_calls, lock_calls);
	vkDestroyDevice(device, nullptr);
	vkDestroyInstance(instance, nullptr);
	return wait_idle_calls == 0 ? 0 : 1;
}
